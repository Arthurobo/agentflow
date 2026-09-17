package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseAssetName(t *testing.T) {
	cases := []struct {
		version, goos, goarch, want string
		wantErr                     bool
	}{
		{"v1.2.3", "linux", "amd64", "agentflow_1.2.3_linux_amd64.tar.gz", false},
		{"1.2.3", "linux", "arm64", "agentflow_1.2.3_linux_arm64.tar.gz", false},
		{"v0.6.0-rc.1", "darwin", "arm64", "agentflow_0.6.0-rc.1_darwin_arm64.tar.gz", false},
		{"v1.0.0", "darwin", "amd64", "agentflow_1.0.0_darwin_amd64.tar.gz", false},
		{"v1.0.0", "windows", "amd64", "agentflow_1.0.0_windows_amd64.zip", false},
		{"v1.0.0", "windows", "arm64", "", true},
		{"v1.0.0", "linux", "386", "", true},
		{"v", "linux", "amd64", "", true},
	}
	for _, c := range cases {
		got, err := releaseAssetName(c.version, c.goos, c.goarch)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("releaseAssetName(%q,%q,%q) = %q, %v", c.version, c.goos, c.goarch, got, err)
		}
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeRelease struct {
	files map[string][]byte
	hits  []string
}

func (r *fakeRelease) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.hits = append(r.hits, req.URL.Path)
	if req.URL.Path == "/api/latest" {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
		return
	}
	b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/dl/v9.9.9/")]
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(b)
}

func newUpdateFixture(t *testing.T, tamper bool) (*updater, *fakeRelease, string, *int) {
	t.Helper()
	asset := "agentflow_9.9.9_linux_amd64.tar.gz"
	archive := tarGz(t, map[string]string{"agentflow": "NEW BINARY", "README.md": "readme"})
	sum := sha256.Sum256(archive)
	if tamper {
		archive = tarGz(t, map[string]string{"agentflow": "EVIL BINARY"})
	}
	rel := &fakeRelease{files: map[string][]byte{
		asset:           archive,
		"checksums.txt": []byte(fmt.Sprintf("%s  agentflow_9.9.9_linux_arm64.tar.gz\n%s  %s\n", strings.Repeat("0", 64), hex.EncodeToString(sum[:]), asset)),
	}}
	srv := httptest.NewServer(rel)
	t.Cleanup(srv.Close)

	target := filepath.Join(t.TempDir(), "agentflow")
	if err := os.WriteFile(target, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	u := &updater{
		out:       &bytes.Buffer{},
		client:    srv.Client(),
		latestURL: srv.URL + "/api/latest",
		base:      func(tag string) string { return srv.URL + "/dl/" + tag },
		goos:      "linux",
		goarch:    "amd64",
		target:    target,
		current:   "v1.0.0",
		restart:   func(context.Context) error { restarts++; return nil },
	}
	return u, rel, target, &restarts
}

func TestUpdateVerifiesAndReplacesBinary(t *testing.T) {
	u, _, target, restarts := newUpdateFixture(t, false)
	if err := u.run(context.Background(), ""); err != nil {
		t.Fatalf("update: %v\n%s", err, u.out)
	}
	got, _ := os.ReadFile(target)
	fi, _ := os.Stat(target)
	if string(got) != "NEW BINARY" || fi.Mode().Perm() != 0o755 {
		t.Fatalf("target = %q mode %v", got, fi.Mode().Perm())
	}
	if *restarts != 1 {
		t.Fatalf("restarts = %d", *restarts)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".agentflow-update-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}

func TestUpdateRejectsTamperedArchive(t *testing.T) {
	u, _, target, restarts := newUpdateFixture(t, true)
	err := u.run(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD BINARY" || *restarts != 0 {
		t.Fatalf("tampered update touched the binary: %q restarts=%d", got, *restarts)
	}
}

func TestUpdateRequiresChecksumEntry(t *testing.T) {
	u, rel, target, _ := newUpdateFixture(t, false)
	rel.files["checksums.txt"] = []byte(strings.Repeat("a", 64) + "  something-else.tar.gz\n")
	if err := u.run(context.Background(), "v9.9.9"); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD BINARY" {
		t.Fatal("binary replaced without a checksum")
	}
}

func TestUpdateWithCosignAbortsOnBadOrMissingSignature(t *testing.T) {
	u, rel, target, _ := newUpdateFixture(t, false)
	called := 0
	u.cosign = func(context.Context, string, string, string) error {
		called++
		return errors.New("no matching signatures")
	}

	// Signature files missing: abort, don't fall back to checksum-only.
	if err := u.run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "signature could not be downloaded") {
		t.Fatalf("missing signature err = %v", err)
	}
	rel.files["checksums.txt.sig"] = []byte("sig")
	rel.files["checksums.txt.pem"] = []byte("pem")
	if err := u.run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "signature verification") {
		t.Fatalf("bad signature err = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD BINARY" || called != 1 {
		t.Fatalf("binary %q cosign calls %d", got, called)
	}

	u.cosign = func(_ context.Context, blob, sig, cert string) error {
		if filepath.Base(blob) != "checksums.txt" || filepath.Base(sig) != "checksums.txt.sig" || filepath.Base(cert) != "checksums.txt.pem" {
			return fmt.Errorf("wrong files %s %s %s", blob, sig, cert)
		}
		return nil
	}
	if err := u.run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "NEW BINARY" {
		t.Fatalf("binary = %q", got)
	}
}

func TestUpdateSkipsCurrentVersion(t *testing.T) {
	u, rel, target, _ := newUpdateFixture(t, false)
	u.current = "9.9.9"
	if err := u.run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD BINARY" || len(rel.hits) != 1 {
		t.Fatalf("binary %q hits %v", got, rel.hits)
	}
}

func TestExtractBinaryIgnoresOtherEntries(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, tarGz(t, map[string]string{"sub/agentflow": "nested", "README.md": "x"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractBinary(archive, filepath.Join(dir, "out")); err == nil {
		t.Fatal("extracted a nested file as the binary")
	}
}
