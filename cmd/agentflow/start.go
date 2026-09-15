package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/cloud"
	"github.com/arthurobo/agentflow/internal/remote"
)

// startFlow is `agentflow start`: offer the optional email sign-in, install
// and start the service, wait for the daemon, follow remote access as it comes
// up (the Cloudflare tunnel needs nothing from the person; Tailscale may need
// an install, sign-in, permission or approval), wait until the public URL
// answers, then pair a phone and watch for access requests.
type startFlow struct {
	in  io.Reader
	out io.Writer
	cfg config
	// interactive is true when someone at a terminal can answer prompts.
	interactive bool
	// email and noEmail are the --email and --no-email flags.
	email   string
	noEmail bool
	cloud   cloud.Client
	svc     serviceManager
	admin   *adminsock.Client
	spec    serviceSpec
	envFile string
	// open tries to open a URL in a desktop browser.
	open func(url string) error
	// probe checks that the daemon answers at a public URL from here.
	probe func(ctx context.Context, url string) error
	// watch, when set, asks about access requests once a pairing link is
	// shown, until ctx is done. Nil when nobody can answer a prompt.
	watch func(ctx context.Context) error

	poll       time.Duration
	socketWait time.Duration // for the daemon to answer after the service starts
	statusWait time.Duration // for remote access to come up
	healthWait time.Duration // for the public URL to resolve and get a certificate
}

func runStart(cfg config, inv invocation) int {
	svc := newServiceManager()
	if !svc.Supported() {
		fmt.Fprintln(os.Stderr, "agentflow:", errNoServiceManager)
		return 1
	}
	exe, err := resolveExecutable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow: cannot find this binary's path:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	f := &startFlow{
		in:          os.Stdin,
		out:         os.Stdout,
		cfg:         cfg,
		interactive: stdinIsTerminal(),
		email:       inv.Email,
		noEmail:     inv.NoEmail,
		cloud:       cloud.NewClient(cloud.BaseURLFromEnv(os.Getenv)),
		svc:         svc,
		admin:       adminClient(cfg),
		spec:        serviceSpec{ExecPath: exe, PATH: currentPATH()},
		envFile:     envFilePath(),
		open:        openInBrowser,
		probe:       probeHealth,
		poll:        time.Second,
		socketWait:  10 * time.Second,
		statusWait:  10 * time.Minute,
		healthWait:  3 * time.Minute,
	}
	if stdinIsTerminal() {
		f.watch = func(ctx context.Context) error {
			return watchAccessRequests(ctx, os.Stdin, os.Stdout, f.admin, accessPoll)
		}
	}
	if err := f.run(ctx); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stdout)
			fmt.Fprintln(os.Stdout, "Stopped watching. agentflow keeps running in the background;")
			fmt.Fprintln(os.Stdout, "check on it with `agentflow remote status` and pair a phone with `agentflow pair`.")
			return 0
		}
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		return 1
	}
	return 0
}

func (f *startFlow) run(ctx context.Context) error {
	if err := ensureEnvFile(f.envFile); err != nil {
		return err
	}
	signedIn, err := f.signIn(ctx)
	if err != nil {
		return err
	}
	changed, err := f.svc.Install(ctx, f.spec)
	if err != nil {
		return fmt.Errorf("install the service: %w", err)
	}
	if changed {
		fmt.Fprintln(f.out, "Installed the agentflow service; it starts again at login.")
	} else {
		fmt.Fprintln(f.out, "agentflow service is installed and started.")
	}
	fmt.Fprintf(f.out, "  settings  %s\n", f.envFile)
	if !f.svc.StaysUpAfterLogout(ctx) {
		fmt.Fprintln(f.out, "  note      user services stop when you log out; to keep agentflow running, run: loginctl enable-linger $USER")
	}

	if err := f.waitForDaemon(ctx); err != nil {
		return err
	}
	if signedIn {
		// The daemon may have started before the account existed.
		var state accountState
		if err := f.admin.Do(ctx, "POST", "/account/reload", &state); err != nil {
			fmt.Fprintf(f.out, "  note      agentflow didn't pick up the sign-in (%v); run `agentflow restart` so it reports this machine's link\n", err)
		}
	}
	fmt.Fprintln(f.out, "(Ctrl-C is safe at any point: agentflow keeps running in the background.)")
	fmt.Fprintln(f.out)

	st, ok, err := f.awaitRemote(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(f.out)
		fmt.Fprintf(f.out, "Remote access still isn't ready (%s) after %s. agentflow keeps trying in the background.\n", st.State, f.statusWait)
		fmt.Fprintln(f.out, "Check `agentflow remote status`, then run `agentflow pair` when it says running.")
		return nil
	}
	if st.State == remote.StateRunning {
		f.awaitReachable(ctx, st.PublicURL)
	}
	fmt.Fprintln(f.out)
	watch, err := pairFlow(ctx, f.out, f.cfg, f.admin)
	if err != nil || !watch || f.watch == nil {
		return err
	}
	fmt.Fprintln(f.out)
	fmt.Fprintln(f.out, "Watching for access requests; press Ctrl-C to stop.")
	return f.watch(ctx)
}

// signIn offers the email sign-in unless an account is already stored, the
// person opted out, remote access is off, or nobody can answer (without
// --email). It is optional: an empty answer skips it. A sign-in that fails
// doesn't stop the start: agentflow works without an account, and
// `agentflow account login` can finish it later. It reports whether an
// account was saved.
func (f *startFlow) signIn(ctx context.Context) (bool, error) {
	if f.cfg.transport() == "" || f.noEmail {
		return false, nil
	}
	if acct, err := cloud.LoadAccount(f.cfg.dataDir); err == nil && acct != nil {
		fmt.Fprintf(f.out, "Signed in as %s; this machine's link is reported to your account.\n\n", acct.Email)
		return false, nil
	}
	in := f.in
	switch {
	case f.email != "":
		// The address comes from the flag; the code still comes from the
		// person.
		in = io.MultiReader(strings.NewReader(f.email+"\n"), f.in)
	case !f.interactive:
		return false, nil
	}
	acct, err := cloud.PromptEmail(ctx, in, f.out, f.cloud, f.cfg.dataDir, false)
	switch {
	case err == nil:
		fmt.Fprintf(f.out, "Signed in as %s.\n\n", acct.Email)
		return true, nil
	case errors.Is(err, cloud.ErrSkipped):
		fmt.Fprintln(f.out)
		return false, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	}
	fmt.Fprintln(f.out, signInSkipped(err))
	fmt.Fprintln(f.out)
	return false, nil
}

// signInSkipped is the one line `agentflow start` prints when the optional
// sign-in didn't work. When the account service can't be reached (no network,
// or the service failing with a 5xx) it says only that, without the transport
// error's detail; otherwise it gives the reason (too many codes, wrong codes).
func signInSkipped(err error) string {
	var apiErr *cloud.APIError
	var netErr net.Error
	if errors.As(err, &netErr) || (errors.As(err, &apiErr) && apiErr.Status >= 500) {
		return "Email sign-in skipped: the account service isn't reachable. Run `agentflow account login` later."
	}
	return fmt.Sprintf("Email sign-in skipped (%v). Run `agentflow account login` to try again.", err)
}

// waitForDaemon polls the admin socket until the daemon answers.
func (f *startFlow) waitForDaemon(ctx context.Context) error {
	deadline := time.Now().Add(f.socketWait)
	for {
		var h map[string]any
		err := f.admin.Do(ctx, "GET", "/health", &h)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service started but agentflow did not answer on %s within %s (%v); see %s",
				f.admin.Path(), f.socketWait, err, f.svc.LogHint())
		}
		if err := sleepCtx(ctx, f.poll); err != nil {
			return err
		}
	}
}

// awaitRemote follows remote status until it is running (or off), printing
// what the person has to do at each step. ok is false when statusWait ran out.
func (f *startFlow) awaitRemote(ctx context.Context) (st remote.Status, ok bool, err error) {
	deadline := time.Now().Add(f.statusWait)
	var last remote.Status
	shown := false
	opened := map[string]bool{}
	lostContact := false
	for {
		cur, ferr := fetchRemoteStatus(ctx, f.admin)
		switch {
		case ferr != nil && ctx.Err() != nil:
			return last, false, ctx.Err()
		case ferr != nil:
			if !lostContact {
				fmt.Fprintf(f.out, "Lost contact with agentflow (%v); retrying...\n", ferr)
				lostContact = true
			}
		default:
			lostContact = false
			if !shown || !sameStatus(cur, last) {
				f.describe(cur, opened)
				last, shown = cur, true
			}
			if cur.State == remote.StateOff || (cur.State == remote.StateRunning && cur.PublicURL != "") {
				return cur, true, nil
			}
		}
		if time.Now().After(deadline) {
			return last, false, nil
		}
		if err := sleepCtx(ctx, f.poll); err != nil {
			return last, false, err
		}
	}
}

func sameStatus(a, b remote.Status) bool {
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return a == b
}

// describe prints one status change, opening each sign-in or approval link
// in a desktop browser the first time it appears.
func (f *startFlow) describe(st remote.Status, opened map[string]bool) {
	describeRemote(f.out, st, func(link string) { f.showLink(link, opened) })
}

// describeRemote prints what remote access is doing and what, if anything,
// the person has to do about it. showLink prints a link they must open.
func describeRemote(w io.Writer, st remote.Status, showLink func(link string)) {
	switch st.State {
	case remote.StateOff:
		fmt.Fprintln(w, "Remote access is off (AF_REMOTE=off); pairing works on this computer only.")
	case remote.StateStarting:
		fmt.Fprintf(w, "Starting %s...\n", remote.TransportLabel(st.Transport))
	case remote.StateNeedsInstall:
		if st.Error != "" {
			fmt.Fprintf(w, "%s. Start it, or install it from:\n", st.Error)
		} else {
			fmt.Fprintln(w, "Tailscale isn't installed on this computer. Install it and sign in, then agentflow uses it on its own:")
		}
		showLink(st.InstallURL)
		fmt.Fprintln(w, "(Or set AF_TAILSCALE=embedded in the settings file to use agentflow's built-in Tailscale instead.)")
	case remote.StateNeedsPermission:
		fmt.Fprintln(w, "One-time setup: let agentflow manage this computer's Tailscale so it can publish")
		fmt.Fprintln(w, "its own address (no root needed after this). Run:")
		fmt.Fprintf(w, "\n  %s\n\n", st.Command)
		fmt.Fprintln(w, "It's a one-time grant — agentflow keeps checking and continues on its own once it's done.")
	case remote.StateNeedsLogin:
		if st.AuthURL == "" && st.Command != "" {
			fmt.Fprintln(w, "This computer's Tailscale is signed out or stopped. Sign in with:")
			fmt.Fprintf(w, "\n  %s\n\n", st.Command)
			return
		}
		if st.AuthURL == "" {
			fmt.Fprintln(w, "Waiting for Tailscale to provide a sign-in link...")
			return
		}
		if st.AuthURL == remote.MachineAuthURL {
			fmt.Fprintln(w, "This machine is waiting for approval by your tailnet's admin. Approve it here:")
		} else {
			fmt.Fprintln(w, "Sign in to Tailscale to reach agentflow from your phone.")
			fmt.Fprintln(w, "Use your own Tailscale account; open this link (or scan it with your phone):")
		}
		showLink(st.AuthURL)
	case remote.StateNeedsFunnelApproval:
		fmt.Fprintln(w, "One click to allow public HTTPS (Tailscale Funnel) for this machine.")
		fmt.Fprintln(w, "It gives agentflow a public https:// address your phone can open without any app (only paired devices get in):")
		showLink(st.ApproveURL)
	case remote.StateError:
		fmt.Fprintf(w, "Remote access hit a problem and will retry: %s\n", st.Error)
	case remote.StateRunning:
		fmt.Fprintf(w, "Remote URL: %s (%s)\n", st.PublicURL, transportDetail(st))
		if st.Transport == remote.TransportCloudflare {
			fmt.Fprintln(w, "Traffic to this address passes through Cloudflare, which can see it; only paired devices get past the pairing page.")
		}
	default:
		fmt.Fprintf(w, "Remote access: %s\n", st.State)
	}
}

// transportDetail names how a running remote reaches the internet:
// "Tailscale funnel".
func transportDetail(st remote.Status) string {
	if st.Mode != "" {
		return remote.TransportLabel(st.Transport) + " " + string(st.Mode)
	}
	return remote.TransportLabel(st.Transport)
}

// printLink prints a link on its own line followed by its terminal QR code.
func printLink(w io.Writer, link string) {
	fmt.Fprintf(w, "\n  %s\n\n", link)
	writeQR(w, link)
}

func (f *startFlow) showLink(link string, opened map[string]bool) {
	printLink(f.out, link)
	if opened[link] || f.open == nil {
		return
	}
	opened[link] = true
	if err := f.open(link); err == nil {
		fmt.Fprintln(f.out, "(Opened it in your browser.)")
	}
}

// awaitReachable polls the public URL's health endpoint from this machine so
// the pairing QR is only shown once the phone can actually load it: a new
// ts.net name can take minutes to resolve, and its certificate to be issued.
func (f *startFlow) awaitReachable(ctx context.Context, publicURL string) {
	deadline := time.Now().Add(f.healthWait)
	said := false
	for {
		err := f.probe(ctx, publicURL)
		if err == nil {
			if said {
				fmt.Fprintln(f.out, "It's reachable.")
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !said {
			fmt.Fprintln(f.out, "Waiting for the address to become reachable (new addresses can take a few minutes to resolve and get a certificate)...")
			said = true
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(f.out, "Still not reachable from this machine after %s (%v). The link below may need a few more minutes.\n", f.healthWait, err)
			return
		}
		if err := sleepCtx(ctx, f.poll); err != nil {
			return
		}
	}
}

// probeHealth fetches <base>/api/v1/agentd/health once.
func probeHealth(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/v1/agentd/health", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	// ANY HTTP response means the tunnel routes to us and the certificate is
	// valid — the phone can load the URL, which is all "reachable" means. The
	// status code is deliberately ignored: the pairing gate answers 404 to an
	// unpaired probe like this one, and treating that as "not ready" made
	// start wait forever on a URL that was already working.
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var errAborted = errors.New("aborted")
