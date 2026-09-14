package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	releaseRepo = "arthurobo/agentflow"
	// cosignIdentity is the signing identity of this repository's release
	// workflow on a version tag; cosignIssuer is GitHub Actions' OIDC issuer.
	cosignIdentity = `^https://github\.com/arthurobo/agentflow/\.github/workflows/release\.yml@refs/tags/v.*$`
	cosignIssuer   = "https://token.actions.githubusercontent.com"
	checksumsName  = "checksums.txt"
	maxBinarySize  = 512 << 20
)

// releaseAssetName is the archive for a release, as GoReleaser names it by
// default: agentflow_<version without v>_<GOOS>_<GOARCH>.tar.gz. install.sh
// builds exactly the same name.
func releaseAssetName(version, goos, goarch string) (string, error) {
	v := strings.TrimPrefix(version, "v")
	if v == "" {
		return "", errors.New("empty version")
	}
	switch goos {
	case "linux", "darwin":
	default:
		return "", fmt.Errorf("no release builds for %s", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("no release builds for %s/%s", goos, goarch)
	}
	return fmt.Sprintf("agentflow_%s_%s_%s.tar.gz", v, goos, goarch), nil
}

// updater downloads, verifies and installs a release over the running binary.
type updater struct {
	out    io.Writer
	client *http.Client
	// latestURL is the GitHub API endpoint for the latest release.
	latestURL string
	// base returns the download directory for a tag.
	base    func(tag string) string
	goos    string
	goarch  string
	target  string // resolved path of the binary to replace
	current string // the running version
	// cosign verifies checksums.txt against its signature and certificate;
	// nil when cosign is not installed.
	cosign func(ctx context.Context, blob, sig, cert string) error
	// restart restarts the service when it is running.
	restart func(ctx context.Context) error
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

func runUpdate() int {
	exe, err := resolveExecutable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow update: cannot find this binary's path:", err)
		return 1
	}
	svc := newServiceManager()
	u := &updater{
		out:       os.Stdout,
		client:    newHTTPClient(),
		latestURL: "https://api.github.com/repos/" + releaseRepo + "/releases/latest",
		base: func(tag string) string {
			if b := os.Getenv("AGENTFLOW_RELEASE_BASE"); b != "" {
				return strings.TrimRight(b, "/")
			}
			return "https://github.com/" + releaseRepo + "/releases/download/" + tag
		},
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
		target:  exe,
		current: version,
		restart: func(ctx context.Context) error {
			if !svc.Supported() || !svc.Query(ctx).Running {
				return errServiceNotRunning
			}
			return svc.Restart(ctx)
		},
	}
	if p, err := exec.LookPath("cosign"); err == nil {
		u.cosign = cosignVerifier(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := u.run(ctx, os.Getenv("AGENTFLOW_VERSION")); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow update:", err)
		return 1
	}
	return 0
}

var errServiceNotRunning = errors.New("service not running")

// cosignVerifier verifies a blob with the cosign binary at path, keyless,
// pinned to this repository's release workflow.
func cosignVerifier(path string) func(ctx context.Context, blob, sig, cert string) error {
	return func(ctx context.Context, blob, sig, cert string) error {
		cmd := exec.CommandContext(ctx, path, "verify-blob", //nolint:gosec // the cosign binary on PATH, fixed arguments
			"--certificate", cert,
			"--signature", sig,
			"--certificate-identity-regexp", cosignIdentity,
			"--certificate-oidc-issuer", cosignIssuer,
			blob)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

func (u *updater) run(ctx context.Context, version string) error {
	tag := version
	if tag == "" {
		t, err := u.latestTag(ctx)
		if err != nil {
			return fmt.Errorf("find the latest release: %w", err)
		}
		tag = t
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if strings.TrimPrefix(tag, "v") == strings.TrimPrefix(u.current, "v") {
		fmt.Fprintf(u.out, "agentflow %s is already the latest release.\n", tag)
		return nil
	}
	asset, err := releaseAssetName(tag, u.goos, u.goarch)
	if err != nil {
		return err
	}
	base := u.base(tag)

	work, err := os.MkdirTemp("", "agentflow-update-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()

	fmt.Fprintf(u.out, "Downloading %s...\n", asset)
	archive := filepath.Join(work, asset)
	sums := filepath.Join(work, checksumsName)
	if err := u.download(ctx, base+"/"+asset, archive); err != nil {
		return err
	}
	if err := u.download(ctx, base+"/"+checksumsName, sums); err != nil {
		return err
	}
	if u.cosign != nil {
		sig, cert := sums+".sig", sums+".pem"
		if err := u.download(ctx, base+"/"+checksumsName+".sig", sig); err != nil {
			return fmt.Errorf("cosign is installed but the signature could not be downloaded: %w", err)
		}
		if err := u.download(ctx, base+"/"+checksumsName+".pem", cert); err != nil {
			return fmt.Errorf("cosign is installed but the certificate could not be downloaded: %w", err)
		}
		if err := u.cosign(ctx, sums, sig, cert); err != nil {
			return fmt.Errorf("signature verification of %s failed; not installing: %w", checksumsName, err)
		}
		fmt.Fprintln(u.out, "Signature verified.")
	}
	want, err := checksumFor(sums, asset)
	if err != nil {
		return err
	}
	if err := verifySHA256(archive, want); err != nil {
		return fmt.Errorf("%s: %w; not installing", asset, err)
	}
	fmt.Fprintln(u.out, "Checksum verified.")

	newBin := filepath.Join(work, "agentflow")
	if err := extractBinary(archive, newBin); err != nil {
		return err
	}
	if err := replaceFile(u.target, newBin); err != nil {
		return fmt.Errorf("replace %s: %w", u.target, err)
	}
	fmt.Fprintf(u.out, "Installed agentflow %s at %s.\n", tag, u.target)

	switch err := u.restart(ctx); {
	case errors.Is(err, errServiceNotRunning):
		fmt.Fprintln(u.out, "Start it with `agentflow start`.")
	case err != nil:
		return fmt.Errorf("installed, but restarting the service failed: %w", err)
	default:
		fmt.Fprintln(u.out, "Restarted the service.")
	}
	return nil
}

func (u *updater) latestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.latestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", u.latestURL, resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", errors.New("no tag_name in the release")
	}
	return body.TagName, nil
}

func (u *updater) download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxBinarySize)); err != nil {
		_ = f.Close()
		return fmt.Errorf("download %s: %w", url, err)
	}
	return f.Close()
}

// checksumFor returns the SHA-256 listed for name in a checksums file
// ("<hex>  <name>" lines, as sha256sum and GoReleaser write them).
func checksumFor(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != 64 {
				return "", fmt.Errorf("malformed checksum for %s", name)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no entry for %s", checksumsName, name)
}

func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA-256 mismatch (got %s, want %s)", got, want)
	}
	return nil
}

// extractBinary writes the archive's top-level regular file "agentflow" to
// dst.
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
		if strings.TrimPrefix(hdr.Name, "./") != "agentflow" || hdr.Typeflag != tar.TypeReg {
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

// replaceFile atomically replaces dst with a copy of src (mode 0755): the
// copy is written and synced next to dst, then renamed over it, so a crash
// never leaves a half-written binary where the service expects one.
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
