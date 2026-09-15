// Package remote makes the daemon reachable from a phone.
//
// AF_REMOTE=cloudflare (the default) publishes the public handler through a
// Cloudflare named tunnel the agentflow account service provisions for this
// machine, at https://<slug>.useagentflow.xyz (cloudflare.go).
// AF_REMOTE=tailscale publishes it through the Tailscale installed on this
// computer (system.go) or, with AF_TAILSCALE=embedded, through agentflow's
// own tsnet node (tsnet.go): on a Funnel listener (a public HTTPS URL that
// also accepts tailnet peers) or, with AF_REMOTE_MODE=tailnet, tailnet-only.
// AF_REMOTE=off disables remote access entirely.
//
// All of them sit behind the Transport interface so the daemon, the admin
// socket and the CLI treat them alike and can be tested with Fake, without a
// Tailscale or Cloudflare account.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the lifecycle of the remote listener.
type State string

const (
	// StateOff means remote access is disabled (AF_REMOTE=off).
	StateOff State = "off"
	// StateStarting means the tsnet node is starting or reconnecting.
	StateStarting State = "starting"
	// StateNeedsLogin means the user must open AuthURL. A node that is
	// logged in but waiting for a tailnet admin to approve the machine
	// (tailscale's NeedsMachineAuth) is reported the same way, with AuthURL
	// pointing at the admin console's machines page: to the person at the
	// terminal both are "open this link and click approve".
	StateNeedsLogin State = "needs_login"
	// StateNeedsFunnelApproval means the tailnet has not enabled HTTPS
	// certificates and Funnel for this node; ApproveURL is the one-click link.
	StateNeedsFunnelApproval State = "needs_funnel_approval"
	// StateNeedsInstall means the transport's software isn't installed, or
	// isn't running, on this computer; InstallURL says where to get it.
	StateNeedsInstall State = "needs_install"
	// StateNeedsPermission means the transport may not be configured by
	// this user; Command is what fixes it. It is retried on its own.
	StateNeedsPermission State = "needs_permission"
	// StateRunning means the listener is serving and PublicURL is set.
	StateRunning State = "running"
	// StateError means the last attempt failed; the run loop is backing off
	// and will retry on its own. Error says why.
	StateError State = "error"
)

// Mode is funnel (public HTTPS, phone needs no app) or tailnet (private;
// phone needs the Tailscale app).
type Mode string

const (
	ModeFunnel  Mode = "funnel"
	ModeTailnet Mode = "tailnet"
)

// MachineAuthURL is where a tailnet admin approves a new machine.
const MachineAuthURL = "https://login.tailscale.com/admin/machines"

// TailscaleInstallURL is where Tailscale is downloaded.
const TailscaleInstallURL = "https://tailscale.com/download"

// Which Tailscale node serves remote access (Status.Backend).
const (
	// BackendSystem is the Tailscale installed on this computer.
	BackendSystem = "system"
	// BackendEmbedded is agentflow's own tsnet node (AF_TAILSCALE=embedded).
	BackendEmbedded = "embedded"
)

// Status is the snapshot the CLI and the admin socket read.
type Status struct {
	State State `json:"state"`
	// Transport is TransportCloudflare or TransportTailscale; empty when off.
	Transport  string    `json:"transport,omitempty"`
	Mode       Mode      `json:"mode,omitempty"`       // tailscale only
	Backend    string    `json:"backend,omitempty"`    // tailscale only: system or embedded
	InstallURL string    `json:"installUrl,omitempty"` // when needs_install
	Command    string    `json:"command,omitempty"`    // a command that gets past the current state
	AuthURL    string    `json:"authUrl,omitempty"`    // when needs_login
	ApproveURL string    `json:"approveUrl,omitempty"` // when needs_funnel_approval
	PublicURL  string    `json:"publicUrl,omitempty"`  // when running: https://<CertDomains()[0]>
	Error      string    `json:"error,omitempty"`      // when error
	UpdatedAt  time.Time `json:"updatedAt"`
}

// sameAs reports whether two snapshots differ only in UpdatedAt. The run loop
// polls every second; without this every poll would rewrite remote.json and
// wake every subscriber.
func (s Status) sameAs(o Status) bool {
	s.UpdatedAt, o.UpdatedAt = time.Time{}, time.Time{}
	return s == o
}

// Transport is what the daemon runs to be reachable from outside. Run blocks
// until ctx is done; Status and Subscribe are safe from any goroutine.
type Transport interface {
	// Run brings remote access up and serves h on the remote listener once
	// it is ready. It retries failures with backoff and returns only when
	// ctx is done or Close is called.
	Run(ctx context.Context, h http.Handler) error

	// Status returns the current snapshot.
	Status() Status

	// Subscribe returns a channel that receives every status change and a
	// func that cancels the subscription.
	Subscribe() (<-chan Status, func())

	// Logout signs agentflow's embedded node out of the tailnet: it goes back
	// to needs_login with a fresh login URL, and the machine stays listed in
	// the Tailscale admin console until the user removes it there. The
	// system Tailscale, which isn't agentflow's to sign out, and Off return
	// an error.
	Logout(ctx context.Context) error

	// Close stops serving and releases the node. Safe to call more than once.
	Close() error
}

// Off is the AF_REMOTE=off implementation.
type Off struct{}

func (Off) Run(ctx context.Context, _ http.Handler) error { <-ctx.Done(); return nil }
func (Off) Status() Status                                { return Status{State: StateOff} }
func (Off) Subscribe() (<-chan Status, func()) {
	ch := make(chan Status, 1)
	ch <- Off{}.Status()
	return ch, func() {}
}
func (Off) Logout(context.Context) error { return fmt.Errorf("remote access is off (AF_REMOTE=off)") }
func (Off) Close() error                 { return nil }

// statusBus holds the current snapshot and fans changes out to subscribers
// and, optionally, to a status file.
type statusBus struct {
	mu       sync.Mutex
	cur      Status
	subs     []chan Status
	closed   bool
	onChange func(Status)
}

func newStatusBus(initial Status, onChange func(Status)) *statusBus {
	if initial.UpdatedAt.IsZero() {
		initial.UpdatedAt = time.Now()
	}
	b := &statusBus{cur: initial, onChange: onChange}
	if onChange != nil {
		onChange(initial)
	}
	return b
}

// publish records st if it differs from the current snapshot. Slow
// subscribers miss intermediate states rather than blocking the run loop;
// they can always read snapshot().
func (b *statusBus) publish(st Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || st.sameAs(b.cur) {
		return
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	b.cur = st
	for _, s := range b.subs {
		select {
		case s <- st:
		default:
		}
	}
	if b.onChange != nil {
		b.onChange(st)
	}
}

func (b *statusBus) snapshot() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// subscribe returns a channel for future changes. The current snapshot is not
// replayed; callers that want it read snapshot() too.
func (b *statusBus) subscribe() (<-chan Status, func()) {
	ch := make(chan Status, 8)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	b.subs = append(b.subs, ch)
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.subs {
			if s == ch {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				close(ch)
				break
			}
		}
	}
	return ch, cancel
}

func (b *statusBus) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, s := range b.subs {
		close(s)
	}
	b.subs = nil
}

// StatusFileName is the file, inside the data directory, that mirrors the
// current status for tools that cannot reach the admin socket.
const StatusFileName = "remote.json"

// statusFileMirror returns a status bus hook that keeps dataDir's status file
// current, or nil when there is no data directory.
func statusFileMirror(dataDir string, log *slog.Logger) func(Status) {
	if dataDir == "" {
		return nil
	}
	path := filepath.Join(dataDir, StatusFileName)
	return func(st Status) {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return
		}
		if err := WriteStatusFile(path, st); err != nil && log != nil {
			log.Debug("remote: write status file", "err", err)
		}
	}
}

// WriteStatusFile atomically replaces path with st as JSON, mode 0600: a temp
// file in the same directory is written, synced and renamed over the target,
// so a reader never sees a half-written file.
func WriteStatusFile(path string, st Status) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".remote-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
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
	return os.Rename(tmpName, path)
}

// ReadStatusFile reads a status written by WriteStatusFile.
func ReadStatusFile(path string) (Status, error) {
	var st Status
	data, err := os.ReadFile(path) //nolint:gosec // the caller names the status file
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}
