package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/remote"
	"github.com/arthurobo/agentflow/internal/store"
)

// adminClient returns a client for the daemon's admin socket.
func adminClient(cfg config) *adminsock.Client {
	return adminsock.NewClient(adminsock.SocketPath(cfg.dataDir))
}

// fetchRemoteStatus asks the daemon for remote-access status.
func fetchRemoteStatus(ctx context.Context, c *adminsock.Client) (remote.Status, error) {
	var st remote.Status
	err := c.Do(ctx, "GET", "/remote/status", &st)
	return st, err
}

// localBaseURL is the pairing base when remote access isn't running:
// http://127.0.0.1:<port>, or the configured host when AF_ADDR names a
// specific non-loopback address (the daemon doesn't listen on 127.0.0.1
// then).
func localBaseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		_, port, _ = net.SplitHostPort(defaultAddr)
	}
	ip := net.ParseIP(host)
	if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
		return "http://" + net.JoinHostPort(host, port)
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port)
}

// pairingURL builds the link the phone opens. The token rides in the
// fragment, which browsers never send to the server, so it can't end up in
// an access log; the pair page removes it from history after reading it.
func pairingURL(base, token string, expiresAt int64) string {
	return strings.TrimRight(base, "/") + "/pair/#token=" + url.QueryEscape(token) +
		"&expires=" + strconv.FormatInt(expiresAt, 10)
}

// writeQR renders text as a terminal QR code. A failure (text too long) just
// skips the code: the URL is always printed too.
func writeQR(w io.Writer, text string) {
	q, err := qrcode.New(text, qrcode.Low)
	if err != nil {
		return
	}
	fmt.Fprintln(w, q.ToSmallString(false))
}

// printPairing prints the phone pairing link, its QR code and when it
// expires.
func printPairing(w io.Writer, base, token string, expiresAt int64, now time.Time) {
	link := pairingURL(base, token, expiresAt)
	fmt.Fprintln(w, "Scan this with your phone camera to pair it:")
	fmt.Fprintln(w)
	writeQR(w, link)
	fmt.Fprintf(w, "Or open this link on the phone:\n  %s\n\n", link)
	printPairingExpiry(w, expiresAt, now)
	fmt.Fprintf(w, "No camera? Open %s/pair/ on the phone, tap Request access, and approve it here.\n", strings.TrimRight(base, "/"))
}

// printLocalPairing prints a pairing link that only opens on this computer.
// There's no QR code: a phone that scanned it would land on its own
// localhost.
func printLocalPairing(w io.Writer, base, token string, expiresAt int64, now time.Time) {
	fmt.Fprintf(w, "Open this on this computer to pair its browser:\n  %s\n\n", pairingURL(base, token, expiresAt))
	printPairingExpiry(w, expiresAt, now)
}

func printPairingExpiry(w io.Writer, expiresAt int64, now time.Time) {
	exp := time.UnixMilli(expiresAt)
	fmt.Fprintf(w, "The link pairs one device and expires at %s (in %s).\n",
		exp.Format("15:04"), exp.Sub(now).Round(time.Minute))
	fmt.Fprintln(w, "Run `agentflow pair` again for each additional device.")
}

// pairFlow prints a pairing link. With the daemon running it mints through
// the admin socket: a phone link on the public URL when remote access is
// running, a this-computer link when remote access is off, and nothing while
// remote access is still coming up (a localhost link would only mislead
// someone holding a phone), just what it's waiting for. With the daemon
// stopped it mints straight into the database for the local URL.
//
// watch reports whether a link was printed by a running daemon, which is
// when access requests can arrive and are worth watching for.
func pairFlow(ctx context.Context, w io.Writer, cfg config, c *adminsock.Client) (watch bool, err error) {
	st, err := fetchRemoteStatus(ctx, c)
	if errors.Is(err, adminsock.ErrNotRunning) {
		db, err := store.Open(cfg.dbPath)
		if err != nil {
			return false, err
		}
		defer func() { _ = db.Close() }()
		token, expiresAt, err := db.MintPairingToken(ctx, cfg.machineID, agentapi.PairingTTL)
		if err != nil {
			return false, fmt.Errorf("mint a pairing token: %w", err)
		}
		printLocalPairing(w, localBaseURL(cfg.addr), token, expiresAt, time.Now())
		fmt.Fprintln(w)
		if daemonHoldsLock(cfg.dbPath) {
			fmt.Fprintf(w, "agentflow is running but did not answer on %s, so this link uses the local address. Check the daemon log (`agentflow status`).\n",
				adminsock.SocketPath(cfg.dataDir))
			return false, nil
		}
		fmt.Fprintln(w, "agentflow isn't running, so this link won't open yet. Start it with `agentflow start` (or `agentflow serve`).")
		if cfg.remoteOn() {
			fmt.Fprintln(w, "To pair a phone, run `agentflow pair` again once remote access is running.")
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ask the daemon for remote status: %w", err)
	}
	running := st.State == remote.StateRunning && st.PublicURL != ""
	if !running && st.State != remote.StateOff {
		fmt.Fprintf(w, "Remote access isn't running yet (%s), so there's no link a phone can open.\n\n", st.State)
		describeRemote(w, st, func(link string) { printLink(w, link) })
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Run `agentflow pair` again once it's running (check with `agentflow remote status`).")
		return false, nil
	}
	var minted adminsock.MintResponse
	if err := c.Do(ctx, "POST", "/pair/mint", &minted); err != nil {
		return false, fmt.Errorf("mint a pairing token: %w", err)
	}
	if running {
		printPairing(w, st.PublicURL, minted.Token, minted.ExpiresAt, time.Now())
		return true, nil
	}
	printLocalPairing(w, localBaseURL(cfg.addr), minted.Token, minted.ExpiresAt, time.Now())
	return true, nil
}

func runPair(cfg config) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c := adminClient(cfg)
	mintCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	watch, err := pairFlow(mintCtx, os.Stdout, cfg, c)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow pair:", err)
		return 1
	}
	if watch && stdinIsTerminal() {
		fmt.Println()
		fmt.Println("Watching for access requests; press Ctrl-C to stop.")
		_ = watchAccessRequests(ctx, os.Stdin, os.Stdout, c, accessPoll)
	}
	return 0
}

// openInBrowser tries to open u in a desktop browser. Headless machines
// (no display on Linux) are skipped: the QR code and URL are printed anyway.
func openInBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u) //nolint:gosec // fixed opener; the URL is one argument, never a shell string
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no display")
		}
		cmd = exec.Command("xdg-open", u) //nolint:gosec // fixed opener; the URL is one argument, never a shell string
	case "windows":
		// rundll32 opens the default handler without cmd.exe quoting pitfalls.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u) //nolint:gosec // fixed opener; the URL is one argument, never a shell string
	default:
		return errors.New("unsupported platform")
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
