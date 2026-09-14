// uploads_api.go — the endpoint an image attachment is uploaded through.
//
//	POST /api/v1/agentd/sessions/{id}/uploads   multipart/form-data
//
// One image per request, on the same authenticated channel as everything
// else. There is no peer-to-peer path: with no relay in the middle, the
// Tailscale connection the UI already loaded over is the private channel, so
// the bytes ride it like any other request. The run's project and session —
// which decide where the file lands and which quota it counts against — are
// resolved HERE from the spawner, never from anything the client says.
package agentapi

import (
	"errors"
	"net/http"

	"github.com/arthurobo/agentflow/internal/uploads"
)

// uploadFormMemory is how much of the multipart form stays in memory before
// the rest spills to a temp file. The image is small after client-side
// downscaling; the request body is capped separately below.
const uploadFormMemory = 1 << 20

// handleUpload stores one uploaded image and returns its attachment id, which
// the client then names in a prompt turn (see handleControlPrompt).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	sess, err := s.lookupRun(r.Context(), runID)
	if err != nil {
		status, code := ttyErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}

	limits := s.Uploads().Limits()
	// Cap the request body at the layer that can refuse mid-stream, so an
	// oversized upload is cut off rather than fully buffered and then judged.
	// The slack covers the multipart envelope around the file.
	r.Body = http.MaxBytesReader(w, r.Body, limits.MaxBytes+(64<<10))
	if err := r.ParseMultipartForm(uploadFormMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				"the image exceeds the upload size limit")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request",
			"expected multipart/form-data with an image field")
		return
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing image field")
		return
	}
	defer func() { _ = file.Close() }()

	name := header.Filename
	if v := r.FormValue("name"); v != "" {
		name = v
	}

	run := uploads.RunInfo{RunID: runID, SessionID: sess.SessionID, Project: sess.Project}
	att, err := s.Uploads().Ingest(r.Context(), s.st, run, name, r.FormValue("sha256"), header.Size, file)
	if err != nil {
		status, code := uploadErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}
	s.log.Info("uploads: stored attachment", "run", runID, "attachment", att.ID,
		"bytes", att.Bytes, "mime", att.Mime)
	writeJSON(w, http.StatusOK, map[string]any{
		"attachmentId": att.ID, "path": att.Path, "bytes": att.Bytes,
	})
}

// Uploads returns the upload policy gate, built on first use. The daemon's
// upload sweeper must use this same instance so deletions it makes are
// reflected in the disk cap the handler enforces.
func (s *Server) Uploads() *uploads.Store {
	s.uploadsOnce.Do(func() {
		st := uploads.New(uploads.Root(), uploads.LimitsFromEnv())
		// Count what earlier runs of the daemon left on disk, or the global
		// cap starts from zero on every restart.
		st.RecomputeTotal()
		s.uploadStore = st
	})
	return s.uploadStore
}

// uploadErrorStatus maps an uploads sentinel error onto an HTTP status and a
// stable code the client can branch on.
func uploadErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, uploads.ErrNoSession):
		return http.StatusConflict, "no_session"
	case errors.Is(err, uploads.ErrSize):
		return http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, uploads.ErrQuota):
		return http.StatusRequestEntityTooLarge, "quota"
	case errors.Is(err, uploads.ErrRate):
		return http.StatusTooManyRequests, "rate"
	case errors.Is(err, uploads.ErrType):
		return http.StatusBadRequest, "type"
	case errors.Is(err, uploads.ErrHash):
		return http.StatusBadRequest, "hash_mismatch"
	case errors.Is(err, uploads.ErrShort), errors.Is(err, uploads.ErrOverrun):
		return http.StatusBadRequest, "bad_request"
	default:
		return http.StatusInternalServerError, "internal"
	}
}
