// prompt_api.go — the one endpoint a message with attachments goes through.
//
// It takes text and attachment IDS. Never paths, never bytes. The bytes
// were uploaded earlier and are already on this disk; the ids are the only
// handle the client has, and they resolve
// only against the run that owns them. That is what stops a message from
// referencing another run's file, or /etc/passwd.
//
// Delivery then depends on the engine: OpenCode takes native file parts,
// Claude has no file part but reads image files with its Read tool, so the
// host names the paths in the turn instead. Both are real deliveries and the
// response says which one happened.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
)

// maxPromptBody bounds the request. A turn plus a handful of ids is a few
// hundred bytes; 16KB is generous and still nowhere near a payload.
const maxPromptBody = 16 << 10

// maxAttachmentsPerTurn matches the composer's own limit.
const maxAttachmentsPerTurn = 4

// handleControlPrompt sends a user turn, with or without attachments.
func (s *Server) handleControlPrompt(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	var body struct {
		Text          string   `json:"text"`
		AttachmentIDs []string `json:"attachmentIds"`
	}
	// MaxBytesReader turns an oversized body into a read error, which we
	// answer as 413 rather than the generic 400 — the client can act on
	// "too big" and cannot act on "bad request".
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPromptBody))
	if err := dec.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				"prompt body is capped at 16KB; attachments travel over the direct channel, not here")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	if strings.TrimSpace(body.Text) == "" && len(body.AttachmentIDs) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "text or attachmentIds required")
		return
	}
	if len(body.AttachmentIDs) > maxAttachmentsPerTurn {
		writeError(w, http.StatusBadRequest, "bad_request", "too many attachments")
		return
	}

	// Ownership: resolve ONLY against this run. A short result means an id
	// belonged to someone else (or to nothing), and the whole turn is
	// refused rather than being quietly delivered with fewer images than
	// the engineer attached.
	var files []engine.Attachment
	if len(body.AttachmentIDs) > 0 {
		rows, err := s.st.AttachmentsForRun(r.Context(), runID, body.AttachmentIDs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		if len(rows) != len(body.AttachmentIDs) {
			writeError(w, http.StatusForbidden, "forbidden",
				"one or more attachments do not belong to this run")
			return
		}
		for _, a := range rows {
			files = append(files, engine.Attachment{
				ID: a.ID, Path: a.Path, Mime: a.Mime, Bytes: a.Bytes,
				Name: attachmentDisplayName(a.Path),
			})
		}
	}

	c, caps, engineID, err := s.controlClientFor(r.Context(), runID)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Native first when the engine says it can.
	if len(files) > 0 && caps.AttachFiles {
		err := c.PromptWithFiles(ctx, body.Text, files)
		if err == nil {
			s.log.Info("prompt: delivered natively", "run", runID, "engine", engineID,
				"attachments", len(files))
			writeJSON(w, http.StatusOK, map[string]any{
				"engine": engineID, "delivered": "native", "attachments": len(files),
			})
			return
		}
		if !errors.Is(err, engine.ErrUnsupported) {
			writeError(w, http.StatusBadGateway, "control_prompt_failed", err.Error())
			return
		}
		// ErrUnsupported: fall through to the path-in-text turn below. An
		// engine that declared the capability and then declined is still
		// better served by a path than by an error.
		s.log.Info("prompt: engine declined native attachments; naming paths instead",
			"run", runID, "engine", engineID)
	}

	// No attachments and no native path: an ordinary prompt.
	if len(files) == 0 {
		if err := c.Prompt(ctx, body.Text); err != nil {
			if errors.Is(err, engine.ErrUnsupported) {
				writeError(w, http.StatusBadRequest, "control_unsupported",
					"engine "+engineID+" cannot take a prompt right now")
				return
			}
			writeError(w, http.StatusBadGateway, "control_prompt_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID, "delivered": "native", "attachments": 0,
		})
		return
	}

	// Path-in-text: the engine reads the files itself.
	turn := AttachmentTurn(files, body.Text)
	if err := s.submitTurn(runID, turn); err != nil {
		writeError(w, http.StatusBadGateway, "control_prompt_failed", err.Error())
		return
	}
	s.log.Info("prompt: delivered as paths in the turn", "run", runID, "engine", engineID,
		"attachments", len(files))
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": engineID, "delivered": "path-in-text",
		"attachments": len(files), "turn": turn,
	})
}

// AttachmentTurn builds the text a path-reading engine gets.
//
// One "Attached image:" line per file, a blank line, then what the engineer
// wrote. The paths lead so the model has them before the sentence that talks
// about them, and an empty message still says what to do with the images
// rather than delivering a bare path and hoping.
func AttachmentTurn(files []engine.Attachment, text string) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString("Attached image: ")
		b.WriteString(f.Path)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if strings.TrimSpace(text) == "" {
		b.WriteString("Please look at the attached image(s).")
	} else {
		b.WriteString(text)
	}
	return b.String()
}

// submitTurn types a multi-line turn into the run's TUI through the same
// paced submit the composer uses.
//
// Bracketing is not a guess: the hub watches the TUI's own DECSET 2004 and
// reports what it asked for. It matters here more than anywhere — this turn
// is several lines, so unbracketed the first newline would submit it and the
// engineer's text would arrive as a second, orphaned turn.
func (s *Server) submitTurn(runID, turn string) error {
	return s.tty.Submit(runID, turn, s.tty.BracketedPaste(runID))
}

// attachmentDisplayName is the leaf name, for engines that show one.
func attachmentDisplayName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 && i+1 < len(path) {
		return path[i+1:]
	}
	return path
}
