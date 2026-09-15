package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// envFilePath is ~/.config/agentflow/agentflow.env.
func envFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "agentflow", "agentflow.env")
}

// loadEnvFile exports the KEY=VALUE pairs from the env file into the process
// environment, without overriding anything already set: a value given in the
// shell (or by a test) always wins. The daemon reads its settings this way
// both in the foreground and under the service manager, so there is one
// parser for the file.
//
// Blank lines and lines starting with '#' are skipped, surrounding whitespace
// is trimmed, one layer of matching quotes is removed, and $HOME / ${HOME} are
// expanded. A missing file is fine: every setting has a default.
func loadEnvFile(path string) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	home, _ := os.UserHomeDir()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := parseEnvLine(sc.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, expandHome(v, home))
		}
	}
	// Values handed over by a service manager or a shell that did not
	// expand $HOME get the same treatment as the file's.
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "AF_") && strings.Contains(v, "HOME") {
			_ = os.Setenv(k, expandHome(v, home))
		}
	}
}

// parseEnvLine parses one env-file line.
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	k, v, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
	if k == "" || strings.ContainsAny(k, " \t") {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return k, v, true
}

// expandHome replaces ${HOME} and $HOME (not $HOMEDIR) with home.
func expandHome(v, home string) string {
	if home == "" {
		return v
	}
	v = strings.ReplaceAll(v, "${HOME}", home)
	var b strings.Builder
	for {
		i := strings.Index(v, "$HOME")
		if i < 0 {
			b.WriteString(v)
			return b.String()
		}
		end := i + len("$HOME")
		if end < len(v) && isNameChar(v[end]) {
			b.WriteString(v[:end])
			v = v[end:]
			continue
		}
		b.WriteString(v[:i])
		b.WriteString(home)
		v = v[end:]
	}
}

func isNameChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// ensureEnvFile writes the commented default env file if there is none, and
// tightens the mode of an existing one. It never rewrites an existing file's
// content: that is where a person's own settings live.
func ensureEnvFile(path string) error {
	if path == "" {
		return fmt.Errorf("cannot locate the home directory for the env file")
	}
	if _, err := os.Stat(path); err == nil {
		return os.Chmod(path, 0o600)
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeFileAtomic(path, []byte(defaultEnvFile), 0o600)
}

// writeEnvValue sets key=value in the env file, replacing an existing
// uncommented assignment (and dropping duplicates of it) or appending one.
// Comments and every other line are kept. The file is replaced atomically.
func writeEnvValue(path, key, value string) error {
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("%s: value must be a single line", key)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data, err = []byte(defaultEnvFile), nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	out := make([]string, 0, len(lines)+1)
	replaced := false
	for _, l := range lines {
		if k, _, ok := parseEnvLine(l); ok && k == key {
			if replaced {
				continue
			}
			out = append(out, key+"="+value)
			replaced = true
			continue
		}
		out = append(out, l)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}
	return writeFileAtomic(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// writeFileAtomic replaces path with data: a temp file in the same directory
// is written with mode perm, synced, and renamed over path, so a crash leaves
// either the old file or the new one, never a truncated mix.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := bytes.NewReader(data).WriteTo(tmp); err != nil {
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// defaultEnvFile is written to ~/.config/agentflow/agentflow.env the first
// time the service is installed. .env.example in the repository is the same
// text (a test keeps them identical).
const defaultEnvFile = `# agentflow settings.
# agentflow reads this file when it starts; restart it after editing
# (agentflow restart). Keep it private: chmod 600.
# Every setting is optional. The commented values are the defaults.
# $HOME and ${HOME} are expanded.

# --- local listener ------------------------------------------------------
# Where the local web UI and API listen. Loopback by default, so nothing is
# reachable from your network unless you change this deliberately.
# AF_ADDR=127.0.0.1:4344

# --- storage -------------------------------------------------------------
# AF_DB=$HOME/.local/share/agentflow/agentflow.db
# Directory for agentflow's other state (tunnel, Tailscale node, admin socket).
# Defaults to the directory that holds AF_DB.
# AF_DATA_DIR=$HOME/.local/share/agentflow
# AF_UPLOADS_DIR=$HOME/.local/share/agentflow/uploads
# Days of session history to keep in the database.
# AF_RETENTION_DAYS=90

# --- engines -------------------------------------------------------------
# Path to the claude binary when it is not on the service's PATH. agentflow
# does not sign engines in; it runs claude and opencode as already logged in.
# AF_CLAUDE=
# Base URL loop members use to reach the local agent API.
# AF_AGENT_BASE_URL=http://127.0.0.1:4344

# --- remote access -------------------------------------------------------------
# cloudflare (default): a stable https://<name>.useagentflow.xyz address for
#   your phone through a Cloudflare tunnel set up for this machine by the
#   agentflow account service. Nothing to install or sign in to; agentflow
#   downloads a verified cloudflared into its data directory when it isn't on
#   PATH. Cloudflare carries (and can see) the traffic.
# tailscale: serve a URL through your own Tailscale account instead.
# off: local only.
# Machines first set up with Tailscale keep using it until this is set.
# AF_REMOTE=cloudflare
# Loopback address the tunnel (cloudflared, or the system Tailscale) forwards
# public requests to. Cloudflare default: 127.0.0.1:4345, which is where this
# machine's tunnel points, so change it only together with the tunnel.
# Tailscale default: a free port. Never the same port as AF_ADDR.
# AF_TUNNEL_ADDR=

# Tailscale only (AF_REMOTE=tailscale):
# funnel: public HTTPS URL, the phone needs no app (tailnet devices work too).
# tailnet: only devices in your tailnet (phone needs the Tailscale app).
# AF_REMOTE_MODE=funnel
# embedded (default): agentflow runs its own Tailscale node — no install, no
#   root, just a one-time browser sign-in.
# system: use the Tailscale already installed on this computer; its URL is
#   https://<this machine>.<tailnet>.ts.net:8443 and it needs a one-time
#   'sudo tailscale set --operator=$USER'.
# AF_TAILSCALE=embedded
# Embedded node only: machine name in your tailnet. Default: a random
# agentflow-xxxxxx, kept across restarts. Changing it means re-pairing every
# phone.
# AF_TS_HOSTNAME=
# on: allow the Tailscale client to upload its logs to Tailscale.
# AF_TS_LOGS=off

# --- account -----------------------------------------------------------------
# Service that sets up this machine's Cloudflare tunnel and, once you sign in
# with agentflow account login, emails its link and lists it on the dashboard.
# AF_CLOUD_URL=https://cloud.useagentflow.xyz

# --- phone uploads -----------------------------------------------------------
# AF_UPLOADS_MAX_BYTES=10485760
# AF_UPLOADS_SESSION_BYTES=209715200
# AF_UPLOADS_SESSION_FILES=100
# AF_UPLOADS_TOTAL_BYTES=2147483648
# AF_UPLOADS_RATE_PER_MINUTE=20
# AF_UPLOADS_RETENTION_DAYS=7

# --- development -----------------------------------------------------------
# Extra origin allowed to call the local API from a browser (a web dev server).
# AF_DEV_CORS_ORIGIN=
`
