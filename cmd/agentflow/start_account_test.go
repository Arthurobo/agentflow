package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/cloud"
	"github.com/arthurobo/agentflow/internal/remote"
)

// newPromptingStartFlow is a start flow someone answers at a terminal (or a
// script feeds).
func newPromptingStartFlow(t *testing.T, input string, status remote.Status) (*startFlow, *fakeService, *fakeDaemon, *syncBuffer) {
	t.Helper()
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	d := startFakeDaemon(t, dataDir, status)
	out := &syncBuffer{}
	f, svc, _ := newTestStartFlow(t, cfg, out)
	f.in = strings.NewReader(input)
	f.interactive = true
	f.cloud = &fakeCloud{}
	return f, svc, d, out
}

var tailscaleRunning = remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, PublicURL: "https://desk.tail1234.ts.net:8443"}

func TestStartGoesStraightToTailscaleWithAnOptionalEmail(t *testing.T) {
	f, svc, d, out := newPromptingStartFlow(t, "me@example.com\n123456\n", tailscaleRunning)
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	text := out.String()
	acct, err := cloud.LoadAccount(f.cfg.dataDir)
	if err != nil || acct == nil || acct.Email != "me@example.com" {
		t.Fatalf("account = %+v %v", acct, err)
	}
	order := []string{
		"Email for your agentflow link and dashboard (optional, press Enter to skip): ",
		"Code sent to me@example.com",
		"Signed in as me@example.com.",
		"Installed the agentflow service",
		"Remote URL: https://desk.tail1234.ts.net:8443 (Tailscale funnel)",
		"https://desk.tail1234.ts.net:8443/pair/#token=tok123",
	}
	pos := 0
	for _, want := range order {
		i := strings.Index(text[pos:], want)
		if i < 0 {
			t.Fatalf("output lacks %q after position %d:\n%s", want, pos, text)
		}
		pos += i + len(want)
	}
	// There is no transport to choose, and nothing is written for one.
	if strings.Contains(text, "How should your phone") || strings.Contains(text, "Saved AF_REMOTE") {
		t.Fatalf("asked for a transport:\n%s", text)
	}
	data, err := os.ReadFile(f.envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, _, ok := parseEnvLine(line); ok && k == "AF_REMOTE" {
			t.Fatalf("start set AF_REMOTE:\n%s", data)
		}
	}
	if svc.restarts != 0 {
		t.Fatalf("restarts = %d", svc.restarts)
	}
	d.mu.Lock()
	reloads := d.reloads
	d.mu.Unlock()
	if reloads != 1 {
		t.Fatalf("the daemon wasn't told about the new sign-in: reloads=%d", reloads)
	}
}

func TestStartEnterSkipsTheEmail(t *testing.T) {
	f, svc, d, out := newPromptingStartFlow(t, "\n", tailscaleRunning)
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if acct, _ := cloud.LoadAccount(f.cfg.dataDir); acct != nil {
		t.Fatalf("signed in after skipping: %+v", acct)
	}
	text := out.String()
	if !strings.Contains(text, "(optional, press Enter to skip)") || !strings.Contains(text, "/pair/#token=tok123") {
		t.Fatalf("output:\n%s", text)
	}
	if strings.Count(text, "Email for your agentflow link") != 1 {
		t.Fatalf("an empty answer must not be asked again:\n%s", text)
	}
	if d.reloads != 0 || len(svc.installed) != 1 {
		t.Fatalf("reloads=%d installs=%d", d.reloads, len(svc.installed))
	}
}

func TestStartDoesNotAskAgainWhenSignedIn(t *testing.T) {
	f, _, _, out := newPromptingStartFlow(t, "", tailscaleRunning)
	saveTestAccount(t, f.cfg.dataDir, "me@example.com")
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	text := out.String()
	if strings.Contains(text, "Email for your agentflow link") ||
		!strings.Contains(text, "Signed in as me@example.com; this machine's link is reported") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestStartWithoutATerminalNeverWaitsForAnEmail(t *testing.T) {
	for name, noEmail := range map[string]bool{"no flag": false, "--no-email": true} {
		t.Run(name, func(t *testing.T) {
			f, svc, _, out := newPromptingStartFlow(t, "", tailscaleRunning)
			f.interactive = false
			f.noEmail = noEmail
			if err := f.run(context.Background()); err != nil {
				t.Fatalf("start: %v\n%s", err, out.String())
			}
			text := out.String()
			if strings.Contains(text, "Email for") || !strings.Contains(text, "/pair/#token=tok123") || len(svc.installed) != 1 {
				t.Fatalf("output:\n%s", text)
			}
		})
	}
}

func TestStartNoEmailFlagSkipsThePromptAtATerminal(t *testing.T) {
	f, _, _, out := newPromptingStartFlow(t, "me@example.com\n123456\n", tailscaleRunning)
	f.noEmail = true
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "Email for") {
		t.Fatalf("output:\n%s", out.String())
	}
	if acct, _ := cloud.LoadAccount(f.cfg.dataDir); acct != nil {
		t.Fatalf("signed in with --no-email: %+v", acct)
	}
}

func TestStartEmailFlagStillTakesTheCode(t *testing.T) {
	f, _, _, out := newPromptingStartFlow(t, "123456\n", tailscaleRunning)
	f.interactive = false
	f.email = "flag@example.com"
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if acct, _ := cloud.LoadAccount(f.cfg.dataDir); acct == nil || acct.Email != "flag@example.com" {
		t.Fatalf("account = %+v\n%s", acct, out.String())
	}
}

func TestStartEmailFlagWithoutACodeCarriesOn(t *testing.T) {
	f, svc, _, out := newPromptingStartFlow(t, "", tailscaleRunning)
	f.interactive = false
	f.email = "flag@example.com"
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if acct, _ := cloud.LoadAccount(f.cfg.dataDir); acct != nil || len(svc.installed) != 1 {
		t.Fatalf("account = %+v installs=%d\n%s", acct, len(svc.installed), out.String())
	}
}

func TestStartKeepsGoingWhenTheAccountServiceIsDown(t *testing.T) {
	f, _, _, out := newPromptingStartFlow(t, "me@example.com\n", tailscaleRunning)
	f.cloud = &downCloud{}
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "Email sign-in skipped: the account service isn't reachable. Run `agentflow account login` later.\n") ||
		strings.Contains(text, "HTTP 503") || !strings.Contains(text, "/pair/#token=tok123") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestSignInSkippedIsOneQuietLine(t *testing.T) {
	unreachable := []error{
		fmt.Errorf("send a sign-in code: %w", &url.Error{Op: "Post", URL: "https://cloud.useagentflow.xyz/v1/auth/code",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}),
		fmt.Errorf("send a sign-in code: %w", &cloud.APIError{Status: 502, Code: "bad_gateway"}),
	}
	for _, err := range unreachable {
		if got := signInSkipped(err); got != "Email sign-in skipped: the account service isn't reachable. Run `agentflow account login` later." {
			t.Errorf("%v: %q", err, got)
		}
	}
	got := signInSkipped(errors.New("too many sign-in codes were requested for this email; wait a few minutes and try again"))
	if !strings.Contains(got, "too many sign-in codes") || strings.Contains(got, "\n") {
		t.Errorf("a reason from the service must be kept, on one line: %q", got)
	}
}

func TestStartWithRemoteOffOffersNoEmail(t *testing.T) {
	f, _, _, out := newPromptingStartFlow(t, "me@example.com\n123456\n", remote.Status{State: remote.StateOff})
	f.cfg.remoteMode = "off"
	if err := f.run(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "Email for") {
		t.Fatalf("output:\n%s", out.String())
	}
}

type downCloud struct{ fakeCloud }

func (*downCloud) RequestCode(context.Context, string) error {
	return &cloud.APIError{Status: 503, Code: "unavailable", Message: "service unavailable"}
}
