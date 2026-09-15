package uploads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// Registry is the persistence Ingest needs, as an interface so the store
// sequence can be tested against a fake without a database. The rows are
// store.Attachment: one definition of what an attachment is, rather than two
// that have to be kept in step.
type Registry interface {
	InsertAttachment(ctx context.Context, a *store.Attachment) error
	SessionAttachmentUsage(ctx context.Context, sessionID string) (files int64, bytes int64, err error)
	AttachmentsOlderThan(ctx context.Context, cutoffMs int64, limit int) ([]*store.Attachment, error)
	AttachmentsWithoutASession(ctx context.Context, limit int) ([]*store.Attachment, error)
	DeleteAttachment(ctx context.Context, id string) error
}

// RunInfo is what an upload needs to know about the run it belongs to. The
// project and session decide where the file lands and which quota it counts
// against, and are the server's to know — never taken from the client.
type RunInfo struct {
	RunID     string
	SessionID string
	Project   string
}

// ingestBuf is the streaming read size. A normalized screenshot is well under
// a megabyte, so this only ever loops a handful of times.
const ingestBuf = 64 << 10

// Ingest applies the full upload policy and stores one image, returning the
// attachment row. It is the single place the store sequence lives — the HTTP
// upload handler is its only caller.
//
// announced is the exact size of the incoming stream (the multipart part's
// size); the transfer is refused at the byte that would exceed it. wantHash,
// when non-empty, is a lowercase sha256 the bytes must match. On any failure
// nothing is left on disk and any reserved bytes are released.
func (s *Store) Ingest(ctx context.Context, reg Registry, run RunInfo, name, wantHash string, announced int64, r io.Reader) (*store.Attachment, error) {
	// Account uploads against the engine's session id once it exists, but fall
	// back to the run id before then. A brand-new session has no engine session
	// id for the first few seconds, and attaching a screenshot right after
	// starting a session is the most common upload flow — refusing there broke
	// it. The run id is always present and keeps the per-session quota honest
	// (accounted per run until the session id lands). Only a run with neither
	// id has nowhere to account the bytes, so usage keyed on "" would read as
	// zero and bypass the quota — that alone is refused.
	account := run.SessionID
	if account == "" {
		account = run.RunID
	}
	if account == "" {
		return nil, ErrNoSession
	}
	usedFiles, usedBytes, err := reg.SessionAttachmentUsage(ctx, account)
	if err != nil {
		return nil, ErrQuota
	}
	// The pre-transfer checks: announced size, per-session quota, global disk
	// cap and per-run rate — refused before a byte is written.
	if err := s.Allow(run.RunID, announced, usedBytes, usedFiles); err != nil {
		return nil, err
	}
	dest, err := s.NewDest(run.Project, account, name)
	if err != nil {
		return nil, ErrInternal
	}
	file, err := os.OpenFile(dest.Part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, ErrInternal
	}
	discard := func() { _ = file.Close(); _ = os.Remove(dest.Part) }

	hasher := sha256.New()
	head := make([]byte, 0, 512)
	buf := make([]byte, ingestBuf)
	var written int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			// An announced size is a promise, enforced at the byte that
			// crosses the line rather than after the whole stream.
			if written+int64(n) > announced {
				discard()
				return nil, ErrOverrun
			}
			if len(head) < 512 {
				need := 512 - len(head)
				if need > n {
					need = n
				}
				head = append(head, buf[:need]...)
			}
			if _, werr := file.Write(buf[:n]); werr != nil {
				discard()
				return nil, ErrInternal
			}
			_, _ = hasher.Write(buf[:n])
			written += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			discard()
			return nil, ErrInternal
		}
	}
	if written != announced {
		discard()
		return nil, ErrShort
	}
	// The type is decided by the BYTES, not by what the client called them.
	mime := SniffMime(head)
	if mime == "" {
		discard()
		return nil, ErrType
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if want := strings.ToLower(strings.TrimSpace(wantHash)); want != "" && want != sum {
		// A mismatch means the bytes are not what the sender hashed. Keep
		// nothing: a corrupted or substituted file is worse than no file.
		discard()
		return nil, ErrHash
	}
	if err := file.Sync(); err != nil {
		discard()
		return nil, ErrInternal
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(dest.Part)
		return nil, ErrInternal
	}
	// Rename only after the content is verified, so a reader can never see a
	// partial or wrong file at the final path.
	if err := os.Rename(dest.Part, dest.Final); err != nil {
		_ = os.Remove(dest.Part)
		return nil, ErrInternal
	}
	_ = os.Chmod(dest.Final, 0o600)
	// Account the bytes against the global disk cap once the file is durably
	// renamed. A failed insert below removes the file and gives them back.
	s.Reserve(written)

	att := &store.Attachment{
		ID: dest.ID, RunID: run.RunID, SessionID: account,
		Path: dest.Final, Mime: mime, Bytes: written, SHA256: sum,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := reg.InsertAttachment(ctx, att); err != nil {
		if rmErr := os.Remove(dest.Final); rmErr == nil || os.IsNotExist(rmErr) {
			s.Release(written)
		}
		return nil, ErrInternal
	}
	return att, nil
}
