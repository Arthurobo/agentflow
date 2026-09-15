package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/cloud"
	"github.com/arthurobo/agentflow/internal/remote"
	"github.com/arthurobo/agentflow/internal/store"
)

func runStop() int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := newServiceManager().Stop(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		return 1
	}
	fmt.Println("agentflow stopped; it won't start at login until you run `agentflow start`.")
	return 0
}

func runRestart() int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := newServiceManager().Restart(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		return 1
	}
	fmt.Println("agentflow restarted.")
	return 0
}

func describeService(supported bool, st serviceState) string {
	switch {
	case !supported:
		return "not managed on this platform (run `agentflow serve`)"
	case !st.Installed:
		return "not installed (run `agentflow start`)"
	case st.Running:
		return "running"
	case st.Enabled:
		return "installed, not running"
	default:
		return "installed, disabled (run `agentflow start`)"
	}
}

func runStatus(cfg config) int {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	svc := newServiceManager()
	st := svc.Query(ctx)
	fmt.Printf("service     %s\n", describeService(svc.Supported(), st))
	if st.PID > 0 {
		fmt.Printf("pid         %d\n", st.PID)
	}
	fmt.Printf("local url   %s\n", localBaseURL(cfg.addr))
	fmt.Printf("database    %s\n", cfg.dbPath)

	c := adminClient(cfg)
	var health struct {
		Version string `json:"version"`
	}
	switch err := c.Do(ctx, "GET", "/health", &health); {
	case errors.Is(err, adminsock.ErrNotRunning):
		fmt.Println("daemon      not running")
	case err != nil:
		fmt.Printf("daemon      not answering (%v)\n", err)
	default:
		fmt.Printf("daemon      ok (version %s)\n", health.Version)
		if rs, err := fetchRemoteStatus(ctx, c); err == nil {
			printRemoteStatus(os.Stdout, rs, "remote      ")
		}
	}
	if _, info := accountCheck(cfg); info != "" {
		fmt.Printf("account     %s\n", info)
	}
	path, source := servicePATHOrCurrent(svc)
	fmt.Printf("engines     %s (%s)\n", strings.Join(detectEngines(path), ", "), source)
	if st.Installed && !svc.StaysUpAfterLogout(ctx) {
		fmt.Println("linger      off (the service stops when you log out; loginctl enable-linger $USER)")
	}
	return 0
}

// detectEngines reports which coding agents are in pathList.
func detectEngines(pathList string) []string {
	var out []string
	for _, bin := range []string{"claude", "opencode"} {
		if p := lookPathIn(bin, pathList); p != "" {
			out = append(out, bin+" ("+p+")")
		}
	}
	if len(out) == 0 {
		return []string{"none found"}
	}
	return out
}

// printRemoteStatus prints a remote status, one field per line, each line
// starting with prefix-width padding.
func printRemoteStatus(w io.Writer, st remote.Status, first string) {
	pad := strings.Repeat(" ", len(first))
	fmt.Fprintf(w, "%s%s\n", first, st.State)
	if st.Transport != "" && st.State != remote.StateOff {
		fmt.Fprintf(w, "%stransport %s\n", pad, st.Transport)
	}
	if st.Backend != "" && st.State != remote.StateOff {
		fmt.Fprintf(w, "%stailscale %s\n", pad, st.Backend)
	}
	if st.Mode != "" && st.State != remote.StateOff {
		fmt.Fprintf(w, "%smode      %s\n", pad, st.Mode)
	}
	if st.PublicURL != "" {
		fmt.Fprintf(w, "%surl       %s\n", pad, st.PublicURL)
	}
	if st.AuthURL != "" {
		fmt.Fprintf(w, "%ssign in   %s\n", pad, st.AuthURL)
	}
	if st.ApproveURL != "" {
		fmt.Fprintf(w, "%sapprove   %s\n", pad, st.ApproveURL)
	}
	if st.InstallURL != "" {
		fmt.Fprintf(w, "%sinstall   %s\n", pad, st.InstallURL)
	}
	if st.Command != "" {
		fmt.Fprintf(w, "%srun       %s\n", pad, st.Command)
	}
	if st.Error != "" {
		fmt.Fprintf(w, "%serror     %s\n", pad, st.Error)
	}
}

func runRemote(cfg config, verb string) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := remoteCommand(ctx, os.Stdout, cfg, adminClient(cfg), newServiceManager(), envFilePath(), verb); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow remote:", err)
		return 1
	}
	return 0
}

func remoteCommand(ctx context.Context, w io.Writer, cfg config, c *adminsock.Client, svc serviceManager, envFile, verb string) error {
	switch verb {
	case "status":
		st, err := fetchRemoteStatus(ctx, c)
		if errors.Is(err, adminsock.ErrNotRunning) {
			msg := "agentflow is not running (start it with `agentflow start`)"
			if last, ferr := remote.ReadStatusFile(filepath.Join(cfg.dataDir, remote.StatusFileName)); ferr == nil {
				msg += fmt.Sprintf("; last known remote state: %s", last.State)
			}
			return errors.New(msg)
		}
		if err != nil {
			return err
		}
		printRemoteStatus(w, st, "remote    ")
		return nil
	case "logout":
		if err := c.Do(ctx, "POST", "/remote/logout", nil); err != nil {
			if errors.Is(err, adminsock.ErrNotRunning) {
				return errors.New("agentflow is not running; start it first, or delete its Tailscale state with `agentflow uninstall --purge`")
			}
			return err
		}
		fmt.Fprintln(w, "Logged this machine out of Tailscale. Remote access now waits for a new sign-in (see `agentflow remote status`).")
		fmt.Fprintf(w, "The machine is still listed in your tailnet; remove it at %s if you want it gone.\n", remote.MachineAuthURL)
		return nil
	case "on", "off":
		value := defaultTransport(cfg.dataDir)
		if verb == "off" {
			value = "off"
		}
		if err := writeEnvValue(envFile, "AF_REMOTE", value); err != nil {
			return fmt.Errorf("update %s: %w", envFile, err)
		}
		fmt.Fprintf(w, "Set AF_REMOTE=%s in %s.\n", value, envFile)
		if !svc.Supported() || !svc.Query(ctx).Installed {
			fmt.Fprintln(w, "The service isn't installed; the setting applies the next time agentflow starts.")
			return nil
		}
		if err := svc.Restart(ctx); err != nil {
			return fmt.Errorf("restart the service: %w", err)
		}
		fmt.Fprintln(w, "Restarted agentflow.")
		if verb == "on" {
			fmt.Fprintln(w, "Run `agentflow start` to follow sign-in and pair a phone.")
		}
		return nil
	}
	return fmt.Errorf("unknown verb %q", verb)
}

func runDevices(cfg config) int {
	db, err := store.Open(cfg.dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := listDevices(context.Background(), os.Stdout, db, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := listAccessRequests(ctx, os.Stdout, adminClient(cfg), time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow: access requests:", err)
		return 1
	}
	return 0
}

// listDevices prints paired devices. It reads the database directly, which
// works whether or not the daemon is running.
func listDevices(ctx context.Context, w io.Writer, db *store.Store, now time.Time) error {
	devices, err := db.ListDevices(ctx, "device", 200)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Fprintln(w, "No devices paired. Run `agentflow pair` to pair one.")
		return nil
	}
	fmt.Fprintf(w, "%-22s  %-24s  %-8s  %-16s  %s\n", "ID", "NAME", "STATUS", "CREATED", "LAST SEEN")
	for _, d := range devices {
		fmt.Fprintf(w, "%-22s  %-24s  %-8s  %-16s  %s\n",
			d.ID, truncate(d.Name, 24), d.Status, formatMillis(d.CreatedAt), sinceMillis(d.LastSeen, now))
	}
	return nil
}

func runRevoke(cfg config, id string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := revokeDevice(ctx, os.Stdout, cfg, adminClient(cfg), id); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow revoke:", err)
		return 1
	}
	return 0
}

// revokeDevice revokes through the daemon, which also closes the device's
// live sessions. With the daemon stopped there are no live sessions, so the
// database is updated directly.
func revokeDevice(ctx context.Context, w io.Writer, cfg config, c *adminsock.Client, id string) error {
	var out adminsock.RevokeResponse
	err := c.Do(ctx, "POST", "/devices/"+id+"/revoke", &out)
	var apiErr *adminsock.APIError
	switch {
	case err == nil:
		fmt.Fprintf(w, "Revoked %s and closed %d live connection(s). Other devices are unaffected.\n", id, out.ConnectionsClosed)
		return nil
	case errors.As(err, &apiErr) && apiErr.Status == 404:
		return fmt.Errorf("no device with id %q (see `agentflow devices`)", id)
	case !errors.Is(err, adminsock.ErrNotRunning):
		return err
	}
	db, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	d, err := db.GetDeviceByID(ctx, id)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("no device with id %q (see `agentflow devices`)", id)
	}
	if err := db.RevokeDevice(ctx, id); err != nil {
		return err
	}
	if daemonHoldsLock(cfg.dbPath) {
		// Running, but not answering on its admin socket: its periodic token
		// check closes this device's open sessions shortly.
		fmt.Fprintf(w, "Revoked %s (%s). The running daemon did not answer on %s, so its open sessions for this device close within a minute.\n",
			id, d.Name, adminsock.SocketPath(cfg.dataDir))
		return nil
	}
	fmt.Fprintf(w, "Revoked %s (%s). agentflow isn't running, so no live sessions were open.\n", id, d.Name)
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

func sinceMillis(ms int64, now time.Time) string {
	if ms <= 0 {
		return "never"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func runDoctor(cfg config) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := newServiceManager()
	checks := doctorChecks(ctx, cfg, svc, adminClient(cfg), probeHealth)
	ok := true
	for _, c := range checks {
		mark := "PASS"
		if !c.OK {
			mark, ok = "FAIL", false
		}
		fmt.Printf("[%s] %-12s %s\n", mark, c.Name, c.Info)
	}
	if !ok {
		fmt.Println("\nagentflow doctor: some checks failed (see above)")
		return 1
	}
	fmt.Println("\nagentflow doctor: all checks passed")
	return 0
}

// doctorChecks runs the environment checks. Engines are looked up in the PATH
// the service will run with, since that is where they must be found.
func doctorChecks(ctx context.Context, cfg config, svc serviceManager, c *adminsock.Client, probe func(context.Context, string) error) []agentapi.Check {
	var checks []agentapi.Check
	add := func(name string, ok bool, info string) {
		checks = append(checks, agentapi.Check{Name: name, OK: ok, Info: info})
	}

	path, source := servicePATHOrCurrent(svc)
	claudeBin := cfg.claudePath
	if claudeBin == "" {
		claudeBin = lookPathIn("claude", path)
	}
	if claudeBin == "" {
		add("claude", false, "not found in the "+source+"; install Claude Code, then run `agentflow start` again")
	} else if v, err := engineVersion(ctx, claudeBin, path); err != nil {
		add("claude", false, claudeBin+": --version failed: "+err.Error())
	} else {
		add("claude", true, claudeBin+" ("+v+")")
	}
	if p := lookPathIn("opencode", path); p != "" {
		add("opencode", true, p)
	} else {
		add("opencode", true, "not installed (optional)")
	}

	if svc.Supported() {
		st := svc.Query(ctx)
		add("service", st.Running, describeService(true, st))
	}
	if ok, info := accountCheck(cfg); info != "" {
		add("account", ok, info)
	}

	var health map[string]any
	if err := c.Do(ctx, "GET", "/health", &health); err != nil {
		add("daemon", false, "not answering on "+c.Path()+": "+err.Error())
		add("remote", false, "unknown while agentflow is not running")
		return checks
	}
	add("daemon", true, "answering on "+c.Path())

	rs, err := fetchRemoteStatus(ctx, c)
	if err != nil {
		add("remote", false, "status unavailable: "+err.Error())
		return checks
	}
	ok, info := remoteCheck(ctx, rs, probe)
	add("remote", ok, info)
	return checks
}

// accountCheck describes the optional email sign-in for status and doctor.
// Only an unreadable sign-in file is a failure. info is "" when there is
// nothing to say (remote access off and no account).
func accountCheck(cfg config) (ok bool, info string) {
	acct, err := cloud.LoadAccount(cfg.dataDir)
	switch {
	case err != nil:
		return false, "the sign-in file can't be read (" + err.Error() + "); run `agentflow account login`"
	case acct != nil:
		return true, "signed in as " + acct.Email
	case cfg.transport() != "":
		return true, "not signed in (optional: `agentflow account login` emails you this machine's link)"
	}
	return true, ""
}

// remoteCheck turns a remote status into a doctor line. A running remote is
// only healthy if its URL answers from this machine.
func remoteCheck(ctx context.Context, rs remote.Status, probe func(context.Context, string) error) (bool, string) {
	switch rs.State {
	case remote.StateOff:
		return true, "remote access is off (AF_REMOTE=off)"
	case remote.StateRunning:
		info := fmt.Sprintf("%s %s", transportDetail(rs), rs.PublicURL)
		if err := probe(ctx, rs.PublicURL); err != nil {
			return false, info + " is not reachable from this machine yet (" + err.Error() + "); new addresses can take a few minutes"
		}
		return true, info + " (reachable, certificate valid)"
	case remote.StateNeedsLogin:
		if rs.AuthURL == "" && rs.Command != "" {
			return false, "this computer's Tailscale is signed out or stopped; run: " + rs.Command
		}
		if rs.AuthURL == "" {
			return false, "waiting for a Tailscale sign-in link"
		}
		return false, "needs Tailscale sign-in: " + rs.AuthURL
	case remote.StateNeedsInstall:
		info := "Tailscale is not installed: " + rs.InstallURL + " (or set AF_TAILSCALE=embedded)"
		if rs.Error != "" {
			info = rs.Error + "; " + rs.InstallURL
		}
		return false, info
	case remote.StateNeedsPermission:
		return false, "needs permission to change Tailscale serve settings; run: " + rs.Command
	case remote.StateNeedsFunnelApproval:
		return false, "needs Funnel approval: " + rs.ApproveURL
	case remote.StateError:
		return false, remote.TransportLabel(rs.Transport) + " error (retrying): " + rs.Error
	default:
		return false, remote.TransportLabel(rs.Transport) + " " + string(rs.State)
	}
}

// engineVersion runs `bin --version` with the given PATH.
func engineVersion(ctx context.Context, bin, pathList string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version") //nolint:gosec // the engine binary the service will run
	cmd.Env = append(os.Environ(), "PATH="+pathList)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}
