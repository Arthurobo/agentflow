package remote

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// hostnameFile holds the generated node name inside the tailscale state dir.
const hostnameFile = "hostname"

// validHostname matches a DNS label: what Tailscale will accept as a machine
// name without rewriting it.
var validHostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidHostname reports whether name is usable as the node's hostname.
func ValidHostname(name string) bool { return validHostname.MatchString(name) }

// loadOrCreateHostname returns the persisted node name, creating
// agentflow-<6 random hex> on first use.
//
// The name is random because it ends up in public Certificate Transparency
// logs, and a plain "agentflow" advertises that the machine runs a remote
// shell. It is persisted because the phone's pairing lives in browser storage
// scoped to the URL: a new name means every phone has to pair again.
func loadOrCreateHostname(dir string) (string, error) {
	path := filepath.Join(dir, hostnameFile)
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // our own state file
		name := strings.TrimSpace(string(b))
		if ValidHostname(name) {
			return name, nil
		}
		// A hand-edited or corrupted file: replace it rather than hand
		// Tailscale a name it will mangle.
	} else if !os.IsNotExist(err) {
		return "", err
	}
	name, err := newHostname()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(name+"\n"), 0o600); err != nil {
		return "", err
	}
	return name, nil
}

func newHostname() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate hostname: %w", err)
	}
	return "agentflow-" + hex.EncodeToString(b), nil
}

// ensurePrivateDir creates dir with mode 0700, and tightens an existing one:
// it holds the node's private keys.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // a directory needs the search bit
}
