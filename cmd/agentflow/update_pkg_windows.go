//go:build windows

package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// binaryFileName is the agentflow binary's file name inside the release archive
// and on disk.
func binaryFileName() string { return "agentflow.exe" }

// extractBinary writes the zip archive's top-level "agentflow.exe" to dst.
func extractBinary(archive, dst string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, zf := range r.File {
		if strings.TrimPrefix(zf.Name, "./") != binaryFileName() || zf.FileInfo().IsDir() {
			continue
		}
		if zf.UncompressedSize64 == 0 || zf.UncompressedSize64 > uint64(maxBinarySize) {
			return fmt.Errorf("agentflow binary in the archive has an implausible size (%d bytes)", zf.UncompressedSize64)
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(out, rc, int64(zf.UncompressedSize64)); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("%s does not contain an agentflow binary", filepath.Base(archive))
}

// replaceFile installs src over dst. Windows cannot overwrite a running .exe,
// but it can rename it: move the current binary aside to dst+".old", then write
// the new binary at the original path. The leftover .old is cleared on the next
// update (it can't be deleted while the old process is still running).
func replaceFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	old := dst + ".old"
	_ = os.Remove(old) // clear any leftover from a previous update

	renamed := false
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return fmt.Errorf("move running binary aside: %w", err)
		}
		renamed = true
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		if renamed {
			_ = os.Rename(old, dst) // roll back
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		if renamed {
			_ = os.Remove(dst)
			_ = os.Rename(old, dst)
		}
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Remove(old) // best-effort; may still be locked by the old process
	return nil
}
