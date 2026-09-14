package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/cloud"
)

// fakeCloud is a cloud.Client that accepts code 123456 and records logouts.
type fakeCloud struct {
	mu        sync.Mutex
	codes     []string
	logouts   []string
	logoutErr error
}

func (f *fakeCloud) RequestCode(_ context.Context, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = append(f.codes, email)
	return nil
}

func (f *fakeCloud) Verify(_ context.Context, email, code string) (string, error) {
	if code != "123456" {
		return "", &cloud.APIError{Status: 400, Code: "invalid_code"}
	}
	return "token-for-" + email, nil
}

func (f *fakeCloud) Logout(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts = append(f.logouts, token)
	return f.logoutErr
}

func (f *fakeCloud) PutMachine(context.Context, string, cloud.Machine) error { return nil }
func (f *fakeCloud) Heartbeat(context.Context, string, string) error         { return nil }

func newAccountFlow(t *testing.T, input string, running bool) (*accountFlow, *fakeCloud, *bytes.Buffer) {
	t.Helper()
	fc := &fakeCloud{}
	var out bytes.Buffer
	return &accountFlow{
		in:            strings.NewReader(input),
		out:           &out,
		client:        fc,
		baseURL:       "https://cloud.example.com",
		dataDir:       t.TempDir(),
		daemonRunning: func(context.Context) bool { return running },
	}, fc, &out
}

func TestParseAccountCommand(t *testing.T) {
	getenv := func(string) string { return "" }
	for _, verb := range []string{"login", "logout", "status"} {
		got, err := parseCommand([]string{"account", verb}, getenv)
		if err != nil || got != (invocation{Name: "account", AccountVerb: verb}) {
			t.Errorf("account %s: %+v, %v", verb, got, err)
		}
	}
	for _, argv := range [][]string{{"account"}, {"account", "signup"}, {"account", "login", "extra"}, {"account", "--email", "a@example.com"}} {
		if _, err := parseCommand(argv, getenv); !errors.Is(err, errUsage) {
			t.Errorf("%q: err = %v, want a usage error", argv, err)
		}
	}
	if !strings.Contains(usage, "agentflow account login|logout|status") {
		t.Error("usage doesn't mention the account command")
	}
}

func TestAccountLogin(t *testing.T) {
	f, fc, out := newAccountFlow(t, "a@example.com\n123456\n", true)
	if err := f.run(context.Background(), "login"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	acct, err := cloud.LoadAccount(f.dataDir)
	if err != nil || acct == nil || acct.Email != "a@example.com" || acct.Token != "token-for-a@example.com" {
		t.Fatalf("stored account %+v, %v", acct, err)
	}
	id, _ := cloud.MachineID(f.dataDir)
	if acct.MachineID != id {
		t.Fatalf("account machine id %q, MachineID %q", acct.MachineID, id)
	}
	text := out.String()
	for _, want := range []string{
		"Email for your agentflow link and dashboard: ",
		"Code sent to a@example.com.",
		"Signed in as a@example.com.",
		"Your machines: https://cloud.example.com",
		"agentflow restart",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if len(fc.logouts) != 0 {
		t.Fatalf("logouts = %v on a first sign-in", fc.logouts)
	}
}

func TestAccountLoginWhenDaemonStopped(t *testing.T) {
	f, _, out := newAccountFlow(t, "a@example.com\n123456\n", false)
	if err := f.run(context.Background(), "login"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "agentflow restart") || !strings.Contains(out.String(), "the next time it starts") {
		t.Fatalf("output: %s", out)
	}
}

func TestAccountLoginRequiresAnEmail(t *testing.T) {
	f, fc, _ := newAccountFlow(t, "\n", false)
	err := f.run(context.Background(), "login")
	if err == nil || errors.Is(err, cloud.ErrSkipped) {
		t.Fatalf("login with no email: %v", err)
	}
	if acct, _ := cloud.LoadAccount(f.dataDir); acct != nil {
		t.Fatal("an account was stored")
	}
	if len(fc.codes) != 0 {
		t.Fatal("a code was requested without an email")
	}
}

func TestAccountLoginReplacesAndRevokesPrevious(t *testing.T) {
	f, fc, out := newAccountFlow(t, "b@example.com\n123456\n", false)
	old := &cloud.Account{Email: "a@example.com", Token: "old-token", MachineID: "0123456789abcdef0123456789abcdef", VerifiedAt: time.Now()}
	if err := cloud.SaveAccount(f.dataDir, old); err != nil {
		t.Fatal(err)
	}
	if err := f.run(context.Background(), "login"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Signed in as a@example.com. Signing in again replaces") {
		t.Fatalf("output: %s", out)
	}
	if acct, _ := cloud.LoadAccount(f.dataDir); acct.Email != "b@example.com" {
		t.Fatalf("stored account %+v", acct)
	}
	if len(fc.logouts) != 1 || fc.logouts[0] != "old-token" {
		t.Fatalf("logouts = %v, want the previous token revoked", fc.logouts)
	}
}

func TestAccountLogout(t *testing.T) {
	f, fc, out := newAccountFlow(t, "", true)
	if err := f.run(context.Background(), "logout"); err != nil || !strings.Contains(out.String(), "Not signed in.") {
		t.Fatalf("logout while signed out: %v, %s", err, out)
	}

	if err := cloud.SaveAccount(f.dataDir, &cloud.Account{Email: "a@example.com", Token: "tok", MachineID: "m"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := f.run(context.Background(), "logout"); err != nil {
		t.Fatal(err)
	}
	if len(fc.logouts) != 1 || fc.logouts[0] != "tok" {
		t.Fatalf("logouts = %v", fc.logouts)
	}
	if acct, err := cloud.LoadAccount(f.dataDir); acct != nil || err != nil {
		t.Fatalf("account after logout: %+v, %v", acct, err)
	}
	if !strings.Contains(out.String(), "Signed out of a@example.com.") {
		t.Fatalf("output: %s", out)
	}
}

func TestAccountLogoutWhenServiceUnreachable(t *testing.T) {
	f, fc, out := newAccountFlow(t, "", true)
	fc.logoutErr = errors.New("dial tcp: connection refused")
	if err := cloud.SaveAccount(f.dataDir, &cloud.Account{Email: "a@example.com", Token: "tok", MachineID: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := f.run(context.Background(), "logout"); err != nil {
		t.Fatal(err)
	}
	if acct, _ := cloud.LoadAccount(f.dataDir); acct != nil {
		t.Fatal("the local sign-in must be removed even when the service is unreachable")
	}
	for _, want := range []string{"couldn't be reached", "agentflow restart"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q: %s", want, out)
		}
	}
}

func TestAccountStatus(t *testing.T) {
	f, _, out := newAccountFlow(t, "", false)
	if err := f.run(context.Background(), "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not signed in") || !strings.Contains(out.String(), "reporting   off") {
		t.Fatalf("signed out: %s", out)
	}

	id, err := cloud.MachineID(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cloud.SaveAccount(f.dataDir, &cloud.Account{Email: "a@example.com", Token: "tok", MachineID: id, VerifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := f.run(context.Background(), "status"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"account     a@example.com", "machine id  " + id, "dashboard   https://cloud.example.com", "reporting   off (agentflow isn't running)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "tok\n") {
		t.Fatal("status prints the token")
	}

	f.daemonRunning = func(context.Context) bool { return true }
	out.Reset()
	if err := f.run(context.Background(), "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "reporting   on while agentflow runs") {
		t.Fatalf("running daemon: %s", out)
	}
}
