// Package cloudflared runs Cloudflare's cloudflared for this machine's named
// tunnel: it finds or downloads a verified binary, supervises the process
// with the tunnel token, and tells when the tunnel is connected to
// Cloudflare's edge.
package cloudflared

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Version is the cloudflared release agentflow downloads when none is on
// PATH.
//
// To move to a new release:
//  1. Pick the tag from https://github.com/cloudflare/cloudflared/releases.
//  2. Download cloudflared-linux-amd64, cloudflared-linux-arm64,
//     cloudflared-darwin-amd64.tgz and cloudflared-darwin-arm64.tgz from it
//     and run sha256sum on each; compare with the digests GitHub shows for
//     the assets.
//  3. Extract the cloudflared binary from each .tgz and sha256sum it too.
//  4. Check that the release still serves /ready on the --metrics listener
//     (metrics/readiness.go), still logs "Starting metrics server on" and
//     "Registered tunnel connection", still reads TUNNEL_TOKEN and still
//     accepts `tunnel --no-autoupdate --metrics <addr> run`.
//  5. Update Version and every hash in releases below.
const Version = "2026.9.1"

// release is one platform's pinned download.
type release struct {
	asset       string // file name on the GitHub release
	assetSHA256 string
	// binarySHA256 is the hash of the cloudflared executable itself: the
	// asset for plain binaries, the file inside the archive for .tgz assets.
	// An already-downloaded binary is checked against it before every use.
	binarySHA256 string
	tgz          bool
}

var releases = map[string]release{
	"linux/amd64": {
		asset:        "cloudflared-linux-amd64",
		assetSHA256:  "03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc",
		binarySHA256: "03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc",
	},
	"linux/arm64": {
		asset:        "cloudflared-linux-arm64",
		assetSHA256:  "3d97437c71848bd8df68041e12436b484a661d95073ea1937f01a845ce88faa3",
		binarySHA256: "3d97437c71848bd8df68041e12436b484a661d95073ea1937f01a845ce88faa3",
	},
	"darwin/amd64": {
		asset:        "cloudflared-darwin-amd64.tgz",
		assetSHA256:  "ff0d3b51d5ff70eceef89d6b32145fee985018a2174596a5dbe405e2766e2ac4",
		binarySHA256: "1ea07ae775b03236bd6be18ca1848d6bdc4af2f4f3bce398823b5a36e5761b75",
		tgz:          true,
	},
	"darwin/arm64": {
		asset:        "cloudflared-darwin-arm64.tgz",
		assetSHA256:  "c27ab8fd0aa489449e3d201eb02f957ef460a13b613662928b1b23394bf1bcfe",
		binarySHA256: "9a0b19f67dc7a3011bc6b972c7ce06a5fcea8784ac6bd599ffa382ea4aeb5a6e",
		tgz:          true,
	},
	// cloudflared ships a raw .exe for Windows (amd64 only; Windows on ARM runs
	// it under emulation). asset == binary hash since it is not archived.
	"windows/amd64": {
		asset:        "cloudflared-windows-amd64.exe",
		assetSHA256:  "2837888cc0f5d58f15b6dc478376de90b4d3ba5241c7947455d1e0a0df429712",
		binarySHA256: "2837888cc0f5d58f15b6dc478376de90b4d3ba5241c7947455d1e0a0df429712",
	},
}

const releaseBaseURL = "https://github.com/cloudflare/cloudflared/releases/download/"

// maxDownload bounds an asset or an extracted binary; the pinned ones are
// about 40 MB.
const maxDownload = 128 << 20

// Errors.
var (
	ErrChecksum    = errors.New("cloudflared: download doesn't match the pinned SHA-256; refusing to run it")
	ErrUnsupported = errors.New("cloudflared: no pinned download for this platform; install cloudflared on PATH")
)

// Installer finds cloudflared on PATH or downloads the pinned release into
// <dataDir>/bin (~/.local/share/agentflow/bin by default).
type Installer struct {
	dataDir  string
	platform string
	lookPath func(string) (string, error)
	http     *http.Client
	baseURL  string
	releases map[string]release
}

// NewInstaller returns an installer for dataDir on this platform.
func NewInstaller(dataDir string) *Installer {
	return &Installer{
		dataDir:  dataDir,
		platform: runtime.GOOS + "/" + runtime.GOARCH,
		lookPath: exec.LookPath,
		http:     &http.Client{Timeout: 5 * time.Minute},
		baseURL:  releaseBaseURL,
		releases: releases,
	}
}

// Path is where the downloaded binary is kept.
func (i *Installer) Path() string {
	name := "cloudflared"
	if strings.HasPrefix(i.platform, "windows/") {
		name += ".exe"
	}
	return filepath.Join(i.dataDir, "bin", name)
}

// Ensure returns the cloudflared to run. One on PATH wins (the person
// installed and updates it themselves); otherwise the pinned release in
// <dataDir>/bin is verified, and downloaded first if it is missing or doesn't
// match.
func (i *Installer) Ensure(ctx context.Context) (path string, onPATH bool, err error) {
	if p, err := i.lookPath("cloudflared"); err == nil {
		return p, true, nil
	}
	rel, ok := i.releases[i.platform]
	if !ok {
		return "", false, fmt.Errorf("%w (%s)", ErrUnsupported, i.platform)
	}
	dest := i.Path()
	if sum, err := fileSHA256(dest); err == nil && sum == rel.binarySHA256 {
		return dest, false, nil
	}
	if err := i.download(ctx, rel, dest); err != nil {
		return "", false, err
	}
	return dest, false, nil
}

func (i *Installer) download(ctx context.Context, rel release, dest string) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.baseURL+Version+"/"+rel.asset, nil)
	if err != nil {
		return err
	}
	resp, err := i.http.Do(req)
	if err != nil {
		return fmt.Errorf("download cloudflared %s: %w", Version, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download cloudflared %s: HTTP %d", Version, resp.StatusCode)
	}

	asset, err := os.CreateTemp(dir, ".cloudflared-download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(asset.Name()) }()
	defer func() { _ = asset.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(asset, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return fmt.Errorf("download cloudflared %s: %w", Version, err)
	}
	if n > maxDownload || hex.EncodeToString(h.Sum(nil)) != rel.assetSHA256 {
		return fmt.Errorf("%w (%s)", ErrChecksum, rel.asset)
	}

	binPath := asset.Name()
	if rel.tgz {
		if _, err := asset.Seek(0, io.SeekStart); err != nil {
			return err
		}
		bin, err := os.CreateTemp(dir, ".cloudflared-extract-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(bin.Name()) }()
		err = extractBinary(asset, bin)
		if cerr := bin.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("extract cloudflared from %s: %w", rel.asset, err)
		}
		binPath = bin.Name()
	}
	if sum, err := fileSHA256(binPath); err != nil || sum != rel.binarySHA256 {
		return fmt.Errorf("%w (cloudflared in %s)", ErrChecksum, rel.asset)
	}
	if err := os.Chmod(binPath, 0o700); err != nil { //nolint:gosec // an executable only its owner can run
		return err
	}
	// Close the downloaded handle before renaming: Windows refuses to rename a
	// file still open in this process.
	_ = asset.Close()
	// A running cloudflared.exe cannot be overwritten by rename on Windows, but it
	// can be moved aside; the running process keeps the old file open and the next
	// launch uses the new one. (No-op elsewhere and when dest is absent.)
	if runtime.GOOS == "windows" {
		_ = os.Remove(dest + ".old")
		_ = os.Rename(dest, dest+".old")
	}
	return os.Rename(binPath, dest)
}

// extractBinary copies the regular file named cloudflared out of a .tgz.
func extractBinary(r io.Reader, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("archive has no cloudflared file")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "cloudflared" {
			continue
		}
		if hdr.Size > maxDownload {
			return errors.New("cloudflared in the archive is too large")
		}
		_, err = io.Copy(w, io.LimitReader(tr, maxDownload))
		return err
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxDownload+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
