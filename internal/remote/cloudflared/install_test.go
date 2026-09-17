package cloudflared

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func tgz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type releaseServer struct {
	srv   *httptest.Server
	files map[string][]byte
	hits  atomic.Int32
}

func newReleaseServer(t *testing.T, files map[string][]byte) *releaseServer {
	rs := &releaseServer{files: files}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.hits.Add(1)
		name, ok := strings.CutPrefix(r.URL.Path, "/"+Version+"/")
		body, found := rs.files[name]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func testInstaller(t *testing.T, rs *releaseServer, platform string, rels map[string]release) *Installer {
	i := NewInstaller(filepath.Join(t.TempDir(), "data"))
	i.platform = platform
	i.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	i.baseURL = rs.srv.URL + "/"
	i.releases = rels
	return i
}

func TestPinnedReleasesCoverSupportedPlatforms(t *testing.T) {
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		r, ok := releases[p]
		if !ok {
			t.Errorf("no pinned release for %s", p)
			continue
		}
		for _, h := range []string{r.assetSHA256, r.binarySHA256} {
			if b, err := hex.DecodeString(h); err != nil || len(b) != 32 {
				t.Errorf("%s: %q is not a SHA-256", p, h)
			}
		}
		if r.tgz != strings.HasSuffix(r.asset, ".tgz") {
			t.Errorf("%s: tgz flag doesn't match asset %s", p, r.asset)
		}
		if !r.tgz && r.assetSHA256 != r.binarySHA256 {
			t.Errorf("%s: a plain binary's asset and binary hashes must match", p)
		}
	}
}

func TestEnsurePrefersCloudflaredOnPATH(t *testing.T) {
	rs := newReleaseServer(t, nil)
	i := testInstaller(t, rs, "linux/amd64", releases)
	i.lookPath = func(name string) (string, error) { return "/usr/local/bin/" + name, nil }
	path, onPATH, err := i.Ensure(context.Background())
	if err != nil || !onPATH || path != "/usr/local/bin/cloudflared" {
		t.Fatalf("Ensure = %q, %v, %v", path, onPATH, err)
	}
	if rs.hits.Load() != 0 {
		t.Fatal("downloaded although cloudflared is on PATH")
	}
}

func TestEnsureDownloadsAndVerifiesPlainBinary(t *testing.T) {
	bin := []byte("#!/bin/sh\necho fake cloudflared\n")
	rs := newReleaseServer(t, map[string][]byte{"cloudflared-linux-amd64": bin})
	rels := map[string]release{"linux/amd64": {asset: "cloudflared-linux-amd64", assetSHA256: sum(bin), binarySHA256: sum(bin)}}
	i := testInstaller(t, rs, "linux/amd64", rels)

	path, onPATH, err := i.Ensure(context.Background())
	if err != nil || onPATH || path != i.Path() {
		t.Fatalf("Ensure = %q, %v, %v", path, onPATH, err)
	}
	got, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if !bytes.Equal(got, bin) || st.Mode().Perm() != 0o700 {
		t.Fatalf("installed %q mode %v", got, st.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("bin dir has %d entries; temp files left behind", len(entries))
	}

	// Already installed and intact: no second download.
	if _, _, err := i.Ensure(context.Background()); err != nil || rs.hits.Load() != 1 {
		t.Fatalf("second Ensure: err %v, downloads %d", err, rs.hits.Load())
	}

	// Tampered with on disk: replaced by a fresh verified download.
	if err := os.WriteFile(path, []byte("evil"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := i.Ensure(context.Background()); err != nil || rs.hits.Load() != 2 {
		t.Fatalf("after tampering: err %v, downloads %d", err, rs.hits.Load())
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bin) {
		t.Fatal("tampered binary not replaced")
	}
}

func TestEnsureExtractsVerifiedArchive(t *testing.T) {
	bin := []byte("fake darwin cloudflared")
	archive := tgz(t, "cloudflared", bin)
	rs := newReleaseServer(t, map[string][]byte{"cloudflared-darwin-arm64.tgz": archive})
	rels := map[string]release{"darwin/arm64": {asset: "cloudflared-darwin-arm64.tgz", assetSHA256: sum(archive), binarySHA256: sum(bin), tgz: true}}
	i := testInstaller(t, rs, "darwin/arm64", rels)
	path, _, err := i.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bin) {
		t.Fatalf("extracted %q", got)
	}
}

func TestEnsureRefusesUnverifiedDownloads(t *testing.T) {
	bin := []byte("real")
	archive := tgz(t, "cloudflared", []byte("swapped inside"))
	cases := []struct {
		name     string
		platform string
		files    map[string][]byte
		rel      release
	}{
		{"asset hash mismatch", "linux/arm64",
			map[string][]byte{"cloudflared-linux-arm64": []byte("tampered")},
			release{asset: "cloudflared-linux-arm64", assetSHA256: sum(bin), binarySHA256: sum(bin)}},
		{"archive binary mismatch", "darwin/amd64",
			map[string][]byte{"cloudflared-darwin-amd64.tgz": archive},
			release{asset: "cloudflared-darwin-amd64.tgz", assetSHA256: sum(archive), binarySHA256: sum(bin), tgz: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := newReleaseServer(t, tc.files)
			i := testInstaller(t, rs, tc.platform, map[string]release{tc.platform: tc.rel})
			_, _, err := i.Ensure(context.Background())
			if !errors.Is(err, ErrChecksum) {
				t.Fatalf("err = %v, want ErrChecksum", err)
			}
			if _, err := os.Stat(i.Path()); !os.IsNotExist(err) {
				t.Fatal("an unverified binary was installed")
			}
			entries, _ := os.ReadDir(filepath.Dir(i.Path()))
			if len(entries) != 0 {
				t.Fatalf("temp files left: %v", entries)
			}
		})
	}
}

func TestEnsureErrors(t *testing.T) {
	rs := newReleaseServer(t, nil)
	i := testInstaller(t, rs, "windows/arm64", releases) // no pinned download (amd64 runs under emulation)
	if _, _, err := i.Ensure(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported platform: %v", err)
	}
	i = testInstaller(t, rs, "linux/amd64", releases)
	if _, _, err := i.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("missing asset: %v", err)
	}
}
