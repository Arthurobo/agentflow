//go:build unix

package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// binaryFileName is the agentflow binary's file name inside the release archive
// and on disk.
func binaryFileName() string { return "agentflow" }

// extractBinary writes the tar.gz archive's top-level regular file "agentflow"
// to dst.
func extractBinary(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s does not contain an agentflow binary", filepath.Base(archive))
		}
		if err != nil {
			return err
		}
		if strings.TrimPrefix(hdr.Name, "./") != binaryFileName() || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size <= 0 || hdr.Size > maxBinarySize {
			return fmt.Errorf("agentflow binary in the archive has an implausible size (%d bytes)", hdr.Size)
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
}

// replaceFile atomically replaces dst with a copy of src (mode 0755): the copy
// is written and synced next to dst, then renamed over it, so a crash never
// leaves a half-written binary where the service expects one.
func replaceFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".agentflow-update-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
