// Command agentflow is the per-machine daemon that runs and supervises
// interactive coding-agent TUIs (Claude Code and OpenCode) inside PTYs, serves
// the web UI and API on a loopback listener, and makes them reachable from a
// phone through an embedded Tailscale node. There is no hosted relay.
//
// Run `agentflow help` for the subcommands. Settings come from the
// environment and ~/.config/agentflow/agentflow.env; see defaultEnvFile in
// envfile.go for the full list.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/approvals"
	"github.com/arthurobo/agentflow/internal/cloud"
	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/claude"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/httpserve"
	"github.com/arthurobo/agentflow/internal/ingest"
	opencodeingest "github.com/arthurobo/agentflow/internal/ingest/opencode"
	"github.com/arthurobo/agentflow/internal/loopapi"
	"github.com/arthurobo/agentflow/internal/mailapi"
	"github.com/arthurobo/agentflow/internal/remote"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
	"github.com/arthurobo/agentflow/internal/uploads"
	"github.com/arthurobo/agentflow/internal/webui"
)

// version, commit and buildDate are stamped with -ldflags by release builds
// and `make build`.
var (
	version   = "0.5.0-dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// init fills version, commit and buildDate from the module build info when
// the binary was built with `go install` and the ldflags stamps are missing.
func init() {
	if version != "0.5.0-dev" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				commit = s.Value[:7]
			} else if s.Value != "" {
				commit = s.Value
			}
		case "vcs.time":
			if s.Value != "" {
				buildDate = s.Value
			}
		}
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches a command line and returns the exit code.
func run(argv []string) int {
	// Settings from the env file reach every subcommand, in the foreground
	// and under the service manager alike. Values already in the
	// environment win.
	loadEnvFile(envFilePath())
	cloud.Version = version

	inv, err := parseCommand(argv, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
		fmt.Fprintln(os.Stderr)
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if inv.Name == "help" {
		fmt.Print(usage)
		return 0
	}

	cfg, err := loadConfig(inv, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		return 1
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	switch inv.Name {
	case "serve":
		if addrIsNonLoopback(cfg.addr) {
			fmt.Fprintf(os.Stderr, "agentflow: warning: AF_ADDR=%s is not a loopback address, so the local web UI and API are reachable from your network. Every API call still needs a paired device token, but unless you need this, use AF_ADDR=%s. See SECURITY.md.\n", cfg.addr, defaultAddr)
		}
		if err := runServe(logger, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "agentflow:", err)
			return 1
		}
		return 0
	case "start":
		return runStart(cfg, inv)
	case "stop":
		return runStop()
	case "restart":
		return runRestart()
	case "status":
		return runStatus(cfg)
	case "doctor":
		return runDoctor(cfg)
	case "version":
		fmt.Printf("agentflow %s (commit %s, built %s)\n", version, commit, buildDate)
		return 0
	case "update":
		return runUpdate()
	case "uninstall":
		return runUninstall(cfg, inv.Purge, inv.Yes)
	case "pair":
		return runPair(cfg)
	case "devices":
		return runDevices(cfg)
	case "revoke":
		return runRevoke(cfg, inv.Target)
	case "approve":
		return runDecide(cfg, inv.Target, true)
	case "deny":
		return runDecide(cfg, inv.Target, false)
	case "remote":
		return runRemote(cfg, inv.RemoteVerb)
	case "account":
		return runAccount(cfg, inv.AccountVerb)
	case "make-resumable":
		runMakeResumable(logger, cfg, inv.Target)
		return 0
	case "undo-make-resumable":
		runUndoMakeResumable(logger, cfg, inv.Target)
		return 0
	}
	fmt.Fprintf(os.Stderr, "agentflow: unhandled command %q\n", inv.Name)
	return 2
}

type config struct {
	dbPath     string
	addr       string
	claudePath string
	machineID  string
	dataDir    string

	// remoteMode is AF_REMOTE: "tailscale" or "off".
	remoteMode string
	// tunnelAddr is AF_TUNNEL_ADDR, the loopback address the system
	// Tailscale forwards public requests to; "" picks a free port.
	tunnelAddr string
	// remoteTunnel is AF_REMOTE_MODE: "funnel" or "tailnet".
	remoteTunnel remote.Mode
	tsHostname   string
	tsLogs       bool
	// tsEmbedded is AF_TAILSCALE=embedded: agentflow's own tsnet node
	// instead of the Tailscale installed on this computer.
	tsEmbedded bool
}

// remoteOn reports whether remote access is enabled.
func (c config) remoteOn() bool { return c.remoteMode != "off" && c.remoteMode != "" }

// transport is the remote transport name, "" when remote access is off.
func (c config) transport() string {
	if !c.remoteOn() {
		return ""
	}
	return c.remoteMode
}

// loadConfig resolves settings for a parsed command line.
func loadConfig(inv invocation, env func(string) string) (config, error) {
	cfg := config{
		addr:       orDefault(env("AF_ADDR"), defaultAddr),
		dbPath:     env("AF_DB"),
		claudePath: env("AF_CLAUDE"),
		machineID:  hostname(),
		tsHostname: env("AF_TS_HOSTNAME"),
		tsLogs:     env("AF_TS_LOGS") == "on",
	}
	if inv.Name == "serve" {
		cfg.addr, cfg.dbPath = inv.Addr, inv.DB
	}
	if cfg.dbPath == "" {
		p, err := store.DefaultDataPath()
		if err != nil {
			return cfg, err
		}
		cfg.dbPath = p
	}
	cfg.dataDir = orDefault(env("AF_DATA_DIR"), filepath.Dir(cfg.dbPath))

	switch mode := orDefault(env("AF_REMOTE"), "tailscale"); mode {
	case remote.TransportTailscale, "off":
		cfg.remoteMode = mode
	default:
		// A typo must never expose more than intended: treat it as off and
		// say so.
		fmt.Fprintf(os.Stderr, "agentflow: AF_REMOTE=%q is not tailscale or off; remote access is disabled\n", mode)
		cfg.remoteMode = "off"
	}
	if addr := env("AF_TUNNEL_ADDR"); addr != "" {
		// The listener the system Tailscale forwards to trusts forwarding
		// headers, so it must be a loopback port of its own. A bad value
		// falls back to a free loopback port rather than failing every
		// command.
		host, _, err := net.SplitHostPort(addr)
		switch ip := net.ParseIP(host); {
		case err != nil || ip == nil || !ip.IsLoopback():
			fmt.Fprintf(os.Stderr, "agentflow: AF_TUNNEL_ADDR=%q is not a loopback IP address and port; using the default\n", addr)
		case remote.SameListenPort(addr, cfg.addr):
			fmt.Fprintf(os.Stderr, "agentflow: AF_TUNNEL_ADDR=%q is the local listener's address (AF_ADDR); using the default\n", addr)
		default:
			cfg.tunnelAddr = addr
		}
	}
	switch ts := env("AF_TAILSCALE"); ts {
	case "", remote.BackendEmbedded:
		// Embedded (agentflow's own tsnet node) is the default: it needs no
		// Tailscale install and no root — just a one-time browser sign-in —
		// which is the smoothest path on every OS. AF_TAILSCALE=system opts
		// in to the Tailscale already installed on this machine.
		cfg.tsEmbedded = true
	case remote.BackendSystem:
	default:
		fmt.Fprintf(os.Stderr, "agentflow: AF_TAILSCALE=%q is not system or embedded; using the built-in (embedded) Tailscale\n", ts)
		cfg.tsEmbedded = true
	}
	switch tunnel := orDefault(env("AF_REMOTE_MODE"), string(remote.ModeFunnel)); tunnel {
	case string(remote.ModeFunnel), string(remote.ModeTailnet):
		cfg.remoteTunnel = remote.Mode(tunnel)
	default:
		// tailnet is the more private of the two.
		fmt.Fprintf(os.Stderr, "agentflow: AF_REMOTE_MODE=%q is not funnel or tailnet; using tailnet\n", tunnel)
		cfg.remoteTunnel = remote.ModeTailnet
	}
	return cfg, nil
}

// agentBaseURL is where an external agent reaches the member surface. It is
// baked into the prompts handed out at loop creation, so a bare ":4344" listen
// address must not become "http://:4344".
func agentBaseURL(addr string) string {
	if v := os.Getenv("AF_AGENT_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	return "http://" + addr
}

// addrIsNonLoopback reports whether addr binds anything but loopback. An
// empty host (":4344") binds every interface; a hostname is assumed not to
// be loopback so the warning errs on the loud side.
func addrIsNonLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return true
}

func runServe(log *slog.Logger, cfg config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The instance lock comes first: before the database is opened, before
	// the reapers run and before the admin socket replaces a stale socket
	// file. A second daemon on the same database would otherwise reap every
	// run the first one owns.
	lock, err := acquireInstanceLock(cfg.dbPath)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		return err
	}

	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	svc := ingest.New(st, ingest.Options{})

	// Engine registry: claude is always present; opencode only when its
	// binary is installed (a missing binary only fails the actual spawn).
	engines := engine.NewRegistry()
	engines.Register(claude.New(cfg.claudePath))
	var opencodeEng *opencode.Engine
	if _, err := opencode.New(opencode.Config{}).LookupBinary(); err == nil {
		opencodeEng = opencode.New(opencode.Config{})
		engines.Register(opencodeEng)
		log.Info("agentflow: opencode engine registered")
	} else {
		log.Info("agentflow: opencode binary not found; engine not registered")
	}

	// OpenCode ingest: the live event consumer and the storage backfill both
	// write through the single index writer.
	ocIngest := opencodeingest.New(st, svc)
	sp := spawner.New(st, svc, log)
	sp.SetEngines(engines)
	if cfg.claudePath != "" {
		sp.SetClaudePath(cfg.claudePath)
	}

	// Reap what a previous daemon left running before anything serves, and
	// the loops those runs belonged to.
	reapOrphans(context.Background(), st, sp, log)
	reapOrphanLoops(context.Background(), st, sp, log)

	// OpenCode runs: once the TUI is up, pre-create or focus the session through
	// its control API and start the SSE consumer that feeds live events in.
	if opencodeEng != nil {
		sp.SetPostStart(openCodePostStart(st, sp, opencodeEng, ocIngest, log))
	}

	// Index OpenCode sessions started outside agentflow (read-only, cheap
	// when nothing changed).
	go func() {
		report, err := ocIngest.BackfillAll(ctx, "")
		if err != nil {
			log.Warn("opencode: backfill", "err", err)
			return
		}
		if report.Sessions > 0 {
			log.Info("opencode: backfill complete", "sessions", report.Sessions, "parts", report.Parts)
		}
	}()

	api := agentapi.New(st, sp, log, cfg.machineID)
	api.AutoResumable = true
	// The spawner hands every PTY to the terminal hub as soon as the child
	// exists, so an unattended loop member can't block on a full buffer.
	sp.SetTTYRegistrar(func(runID string, master io.ReadWriteCloser, resize func(cols, rows uint16) error, geomCols uint16) {
		api.RegisterTTY(runID, master, resize, geomCols)
	})
	api.Engines = engines
	api.Approvals = approvals.New(st, log, approvals.WithOnOpen(func(a *store.Approval) {
		log.Info("approval: pending", "id", a.ID, "tool", a.ToolName, "mission", a.MissionID)
	}))
	if secret, err := loadOrMintHookSecret(cfg.dataDir); err != nil {
		log.Warn("agentflow: hook secret unavailable; /approvals/request will 401", "err", err)
	} else {
		api.SetHookSecret(secret)
	}

	// Attachments: move a pre-XDG uploads directory into place once, then
	// sweep with the API's own store so quota and sweep agree.
	uploads.MigrateLegacyDir(func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) })
	runUploadSweeper(ctx, st, api.Uploads(), log)

	agentSrv := mailapi.New(st, log, mailapi.Config{})
	loopSrv := loopapi.New(st, agentSrv, log, loopapi.Config{
		BaseURL:    agentBaseURL(cfg.addr),
		Generation: sp.Generation(),
	})
	loopSrv.SetLauncher(newLoopLauncher(sp, engines, cfg.dataDir, log))
	loopSrv.SetModelLister(&loopModels{sp: sp, engines: engines})
	if err := store.WriteDefaultsCopy(cfg.dataDir); err != nil {
		log.Warn("agentflow: could not write the defaults copy", "err", err)
	}
	rep := newReporter(st, api.TTYSubmit, opencodePrompt(sp), log)
	go runSweeper(ctx, st, newNudger(api.TTYSubmit, sp, engines, log), rep, loopSrv.StopRuns, log)
	go newCourier(st, api.TTYSubmit, sp, engines, log).Run(ctx)

	// Two roots from the same servers: loopback serves the local one, the
	// remote listener the public one (no member mail API, no approvals hook).
	localRoot := httpserve.Root(httpserve.Options{Control: api, Agents: agentSrv, Engineer: loopSrv, UI: webui.Handler(), ListenAddr: cfg.addr})
	publicRoot := httpserve.Root(httpserve.Options{Public: true, Control: api, Agents: agentSrv, Engineer: loopSrv, UI: webui.Handler()})

	// The local listener comes up first and never waits on Tailscale.
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.addr, err)
	}
	httpSrv := &http.Server{
		Handler:           localRoot,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	log.Info("agentflow listening", "addr", ln.Addr().String(), "machine", cfg.machineID, "db", cfg.dbPath, "version", version)

	rem := newRemote(cfg, api, log)
	api.SetPublicOrigin(func() string { return rem.Status().PublicURL })
	account := newAccountReporter(cfg, rem, log)
	account.Start(ctx)
	remoteDone := make(chan struct{})
	go func() {
		defer close(remoteDone)
		_ = rem.Run(ctx, publicRoot)
	}()

	admin, err := adminsock.Start(ctx, adminsock.Config{
		Dir:    adminsock.Dir(cfg.dataDir),
		Status: func() any { return rem.Status() },
		Logout: rem.Logout,
		Revoke: func(ctx context.Context, id string) (int, error) {
			d, err := st.GetDeviceByID(ctx, id)
			if err != nil {
				return 0, err
			}
			if d == nil {
				return 0, adminsock.ErrNotFound
			}
			return api.RevokeDevice(ctx, id)
		},
		MintPair:          api.MintPairingToken,
		Health:            func() any { return daemonHealth(sp, cfg, rem, account) },
		PairRequests:      adminPairRequests(api),
		DecidePairRequest: adminDecidePairRequest(api),
		ReloadAccount:     func() any { return account.Reload() },
	})
	if err != nil {
		// The CLI loses `pair`/`remote` against the running daemon, but the
		// daemon itself is fine; don't take every session down for it.
		log.Error("agentflow: admin socket unavailable", "err", err)
	}

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("agentflow: local listener failed", "err", err)
		}
	}
	log.Info("agentflow shutting down")
	stop()
	shutdown(log, ln, httpSrv, sp, api, rem, remoteDone, admin)
	return nil
}

// shutdown stops the daemon in an order that lets agents finish writing:
// stop accepting, stop every agent (10 s budget), close live WebSockets, then
// shut the servers down. The whole sequence fits systemd's TimeoutStopSec=20.
func shutdown(log *slog.Logger, ln net.Listener, httpSrv *http.Server, sp *spawner.Spawner,
	api *agentapi.Server, rem remote.Transport, remoteDone <-chan struct{}, admin *adminsock.Server) {
	_ = ln.Close()
	remoteClosed := make(chan struct{})
	go func() {
		_ = rem.Close()
		close(remoteClosed)
	}()

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := sp.StopAll(stopCtx); err != nil {
		log.Warn("agentflow: stopping agents", "err", err)
	}
	cancel()

	if n := api.CloseAllConnections(); n > 0 {
		log.Info("agentflow: closed live connections", "count", n)
	}
	for _, ch := range []<-chan struct{}{remoteClosed, remoteDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			log.Warn("agentflow: remote access did not stop in time")
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if admin != nil {
		_ = admin.Close()
	}
}

// newAccountReporter reports this machine's link for the stored account.
func newAccountReporter(cfg config, rem remote.Transport, log *slog.Logger) *accountReporter {
	return &accountReporter{
		dataDir:   cfg.dataDir,
		transport: cfg.transport(),
		name:      cfg.machineID,
		version:   version,
		rem:       rem,
		client:    func() cloud.Client { return cloud.NewClient(cloud.BaseURLFromEnv(os.Getenv)) },
		log:       log,
		report:    cloud.RunReporter,
	}
}

// adminPairRequests lists the API's pending access requests for the admin
// socket.
func adminPairRequests(api *agentapi.Server) func() []adminsock.PairRequest {
	return func() []adminsock.PairRequest {
		var out []adminsock.PairRequest
		for _, r := range api.ListPairRequests() {
			out = append(out, adminsock.PairRequest(r))
		}
		return out
	}
}

// adminDecidePairRequest decides access requests from the admin socket (the
// CLI and the prompts in `agentflow start` and `agentflow pair`).
func adminDecidePairRequest(api *agentapi.Server) func(context.Context, string, bool) (adminsock.PairRequest, error) {
	return func(_ context.Context, idOrCode string, approve bool) (adminsock.PairRequest, error) {
		info, err := api.DecidePairRequest(idOrCode, approve, "admin socket")
		if errors.Is(err, agentapi.ErrPairRequestNotFound) {
			return adminsock.PairRequest{}, adminsock.ErrNotFound
		}
		return adminsock.PairRequest(info), err
	}
}

// newRemote builds remote access for cfg. With remote off, remote.json still
// records that, so tools reading it never see a stale "running".
func newRemote(cfg config, api *agentapi.Server, log *slog.Logger) remote.Transport {
	rem, err := remote.Select(remote.SelectConfig{
		Transport: cfg.transport(),
		Tailscale: remote.Config{
			DataDir:       cfg.dataDir,
			Mode:          cfg.remoteTunnel,
			Hostname:      cfg.tsHostname,
			LogsOn:        cfg.tsLogs,
			Embedded:      cfg.tsEmbedded,
			TunnelAddr:    cfg.tunnelAddr,
			LocalAddr:     cfg.addr,
			CloseHijacked: func() { api.CloseAllConnections() },
			Log:           log,
		},
	})
	if err != nil {
		// loadConfig only lets tailscale or off through; this is a
		// programming error, and off is the safe answer to it.
		log.Error("agentflow: remote access disabled", "err", err)
		rem = remote.Off{}
	}
	if _, off := rem.(remote.Off); off {
		if err := remote.WriteStatusFile(filepath.Join(cfg.dataDir, remote.StatusFileName), rem.Status()); err != nil {
			log.Debug("agentflow: write remote status", "err", err)
		}
	}
	return rem
}

// daemonHealth is the admin socket's /health body.
func daemonHealth(sp *spawner.Spawner, cfg config, rem remote.Transport, account *accountReporter) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, _ := sp.List(ctx, "")
	running := 0
	for _, sc := range sess {
		if !sc.State.Terminal() {
			running++
		}
	}
	rs := rem.Status()
	return map[string]any{
		"status":    "ok",
		"version":   version,
		"machineId": cfg.machineID,
		"pid":       os.Getpid(),
		"addr":      cfg.addr,
		"sessions":  map[string]int{"running": running, "total": len(sess)},
		"remote":    map[string]any{"state": rs.State, "transport": rs.Transport, "publicUrl": rs.PublicURL},
		"account":   account.State(),
	}
}

// openCodePostStart binds a freshly started OpenCode TUI to its session
// through the control API and starts the live event stream.
func openCodePostStart(st *store.Store, sp *spawner.Spawner, eng *opencode.Engine, ocIngest *opencodeingest.Ingest, log *slog.Logger) func(context.Context, string, *spawner.Session, spawner.Options) {
	return func(ctx context.Context, runID string, sess *spawner.Session, opts spawner.Options) {
		if opts.Engine != engine.IDOpenCode || sess.ControlBase == "" {
			return
		}
		var boundSessionID string
		// opts.ResumeSessionID makes this a resume: bind that session
		// instead of creating a new one.
		if err := eng.PostStart(ctx, sess.ControlBase, sess.CWD, sess.Title,
			opts.ResumeSessionID, func(sessionID string) {
				boundSessionID = sessionID
				// Through the spawner, so the live session learns the id;
				// writing it onto this hook's copy left the run without it
				// and the run's final persist erased it again.
				if err := sp.SetSessionID(runID, sessionID); err != nil {
					log.Warn("opencode: record session id", "run", runID, "err", err)
				}
			}); err != nil {
			// The TUI is up and typeable; only the control binding failed.
			// The state stays, but the row records why.
			log.Warn("opencode: post-start bind", "run", runID, "base", sess.ControlBase, "err", err)
			_ = st.SetManagedSessionError(ctx, runID,
				"opencode control API never bound at "+sess.ControlBase+": "+err.Error())
			return
		}
		// The consumer is keyed by run and ends with it: the hook's ctx is
		// cancelled when the run exits.
		if err := ocIngest.StartSSE(ctx, runID, sess.ControlBase, boundSessionID); err != nil {
			log.Warn("opencode: start sse", "run", runID, "err", err)
		}
	}
}

// loadOrMintHookSecret returns the approvals hook secret, creating it (32
// random bytes, hex, mode 0600) on first start. A failure is logged by the
// caller and the endpoint rejects every hook rather than accepting one
// unauthenticated.
func loadOrMintHookSecret(dataDir string) ([]byte, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("no data dir")
	}
	path := filepath.Join(dataDir, "hook.secret")
	if b, err := os.ReadFile(path); err == nil {
		if s := bytes.TrimSpace(b); len(s) > 0 {
			return s, nil
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	hexed := []byte(hex.EncodeToString(b))
	if err := os.WriteFile(path, append(hexed, '\n'), 0o600); err != nil {
		return nil, err
	}
	return hexed, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return filepath.Base(h)
}
