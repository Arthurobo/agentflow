package cloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// runPrompt runs PromptEmail on input and returns the account, the output,
// the input left unread and the error.
func runPrompt(t *testing.T, f *fakeService, input string, required bool) (*Account, string, string, error) {
	t.Helper()
	dir := t.TempDir()
	in := strings.NewReader(input)
	var out bytes.Buffer
	acct, err := PromptEmail(context.Background(), in, &out, NewClient(f.srv.URL), dir, required)
	rest, _ := io.ReadAll(in)
	return acct, out.String(), string(rest), err
}

func TestPromptEmailOptionalSkip(t *testing.T) {
	f := newFakeService(t)
	for _, input := range []string{"\n", "   \n", ""} {
		acct, out, _, err := runPrompt(t, f, input, false)
		if !errors.Is(err, ErrSkipped) || acct != nil {
			t.Errorf("input %q: %+v, %v", input, acct, err)
		}
		if !strings.HasPrefix(out, "Email for your agentflow link and dashboard (optional, press Enter to skip): ") {
			t.Errorf("prompt = %q", out)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Fatalf("%d requests for a skipped email", len(f.requests))
	}
}

func TestPromptEmailRequired(t *testing.T) {
	f := newFakeService(t)
	acct, out, _, err := runPrompt(t, f, "\n", true)
	if err == nil || errors.Is(err, ErrSkipped) || acct != nil {
		t.Fatalf("required with only Enter then EOF: %+v, %v", acct, err)
	}
	if !strings.HasPrefix(out, "Email for your agentflow link and dashboard: ") ||
		strings.Count(out, "Email for your agentflow link and dashboard: ") != 2 ||
		!strings.Contains(out, "Type the email to sign in with") {
		t.Fatalf("empty required answer should re-prompt: %q", out)
	}

	// Non-interactive input that ends at the code prompt.
	if _, _, _, err := runPrompt(t, f, "a@example.com\n", true); err == nil || errors.Is(err, ErrSkipped) {
		t.Fatalf("required, EOF at code: %v", err)
	}
	if _, _, _, err := runPrompt(t, f, "a@example.com\n", false); !errors.Is(err, ErrSkipped) {
		t.Fatalf("optional, EOF at code: %v", err)
	}
}

func TestPromptEmailSignsIn(t *testing.T) {
	f := newFakeService(t)
	dir := t.TempDir()
	in := strings.NewReader("not an email\nA@Example.com\n111111\nnext prompt's answer\n")
	var out bytes.Buffer
	acct, err := PromptEmail(context.Background(), in, &out, NewClient(f.srv.URL), dir, true)
	if err != nil {
		t.Fatalf("%v\noutput: %s", err, out.String())
	}
	if acct.Email != "a@example.com" || acct.Token != strings.Repeat("t", 43) || len(acct.MachineID) != 32 || time.Since(acct.VerifiedAt) > time.Minute {
		t.Fatalf("account = %+v", acct)
	}
	for _, want := range []string{"That doesn't look like an email address.", "Code sent to a@example.com. Enter the 6-digit code: ", "Signed in as a@example.com."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// The address is normalized before it goes out, so the code request and
	// the verification name the same account.
	f.mu.Lock()
	sent := append([]string(nil), f.rawEmails...)
	f.mu.Unlock()
	if strings.Join(sent, ",") != "code:a@example.com,verify:a@example.com" {
		t.Fatalf("addresses sent = %v, want the lower-cased address for both calls", sent)
	}
	stored, err := LoadAccount(dir)
	if err != nil || *stored != *acct {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if id, _ := MachineID(dir); id != acct.MachineID {
		t.Fatalf("account machine id %q != MachineID %q", acct.MachineID, id)
	}
	// Nothing past the code line was consumed.
	rest, _ := io.ReadAll(in)
	if string(rest) != "next prompt's answer\n" {
		t.Fatalf("PromptEmail read ahead; left %q", rest)
	}
}

func TestPromptEmailWrongCodesAndResend(t *testing.T) {
	f := newFakeService(t)
	// First code is 111111, the resent one 222222.
	acct, out, _, err := runPrompt(t, f, "a@example.com\n999999\n\nresend\n111111\n222222\n", false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f.mu.Lock()
	sends := f.codeSends["a@example.com"]
	f.mu.Unlock()
	if acct == nil || sends != 2 {
		t.Fatalf("acct %+v, code sends %d", acct, sends)
	}
	if !strings.Contains(out, "That code didn't work.") || !strings.Contains(out, "New code sent to a@example.com.") {
		t.Fatalf("output: %s", out)
	}

	acct, out, rest, err := runPrompt(t, f, "b@example.com\n000000\n000001\n000002\n111111\n", false)
	if !errors.Is(err, ErrTooManyAttempts) || acct != nil {
		t.Fatalf("three wrong codes: %+v, %v\n%s", acct, err, out)
	}
	if rest != "111111\n" {
		t.Fatalf("input after giving up = %q", rest)
	}
}

func TestPromptEmailServiceErrors(t *testing.T) {
	f := newFakeService(t)
	f.setRateLimit(true)
	if _, _, _, err := runPrompt(t, f, "a@example.com\n", false); err == nil || errors.Is(err, ErrSkipped) || !strings.Contains(err.Error(), "too many sign-in codes") {
		t.Fatalf("rate limited: %v", err)
	}
	f.setRateLimit(false)

	acct, out, _, err := runPrompt(t, f, "x@invalid.example\na@example.com\n111111\n", false)
	if err != nil || acct == nil || !strings.Contains(out, "didn't accept that address") {
		t.Fatalf("service-rejected email: %+v, %v\n%s", acct, err, out)
	}

	down := NewClient("http://127.0.0.1:1")
	var buf bytes.Buffer
	_, err = PromptEmail(context.Background(), strings.NewReader("a@example.com\n"), &buf, down, t.TempDir(), false)
	if err == nil || errors.Is(err, ErrSkipped) || !strings.Contains(err.Error(), "send a sign-in code") {
		t.Fatalf("service down: %v", err)
	}
}

func TestPromptEmailCancelWhileWaitingForInput(t *testing.T) {
	f := newFakeService(t)
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := PromptEmail(ctx, pr, io.Discard, NewClient(f.srv.URL), t.TempDir(), true)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PromptEmail didn't return after the context was cancelled")
	}
}
