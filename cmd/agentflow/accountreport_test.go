package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/cloud"
	"github.com/arthurobo/agentflow/internal/remote"
)

// recordingCloud is a cloud.Client that records every machine it is sent.
type recordingCloud struct {
	fakeCloud
	mu   sync.Mutex
	puts []cloud.Machine
}

func (r *recordingCloud) PutMachine(_ context.Context, _ string, m cloud.Machine) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.puts = append(r.puts, m)
	return nil
}

func (r *recordingCloud) machines() []cloud.Machine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cloud.Machine(nil), r.puts...)
}

type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func saveTestAccount(t *testing.T, dataDir, email string) {
	t.Helper()
	if err := cloud.SaveAccount(dataDir, &cloud.Account{Email: email, Token: "tok-" + email, VerifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func newTestReporter(t *testing.T, transport string, rem remote.Transport) (*accountReporter, *recordingCloud, *syncLog) {
	t.Helper()
	rc := &recordingCloud{}
	logs := &syncLog{}
	return &accountReporter{
		dataDir:   t.TempDir(),
		transport: transport,
		name:      "desk",
		version:   "9.9.9",
		rem:       rem,
		client:    func() cloud.Client { return rc },
		log:       slog.New(slog.NewTextHandler(logs, nil)),
		report:    cloud.RunReporter,
	}, rc, logs
}

func waitMachines(t *testing.T, rc *recordingCloud, n int) []cloud.Machine {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := rc.machines(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted %d machine reports, got %+v", n, rc.machines())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAccountReporterReportsTheTransportAndEachNewURL(t *testing.T) {
	rem := remote.NewFake(remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, PublicURL: "https://desk.tail1234.ts.net:8443"})
	a, rc, _ := newTestReporter(t, remote.TransportTailscale, rem)
	saveTestAccount(t, a.dataDir, "me@example.com")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	if st := a.State(); !st.Reporting || st.Email != "me@example.com" {
		t.Fatalf("state = %+v", st)
	}

	first := waitMachines(t, rc, 1)[0]
	id, _ := cloud.MachineID(a.dataDir)
	if first != (cloud.Machine{ID: id, Name: "desk", Transport: "tailscale", URL: "https://desk.tail1234.ts.net:8443", Version: "9.9.9"}) {
		t.Fatalf("first report = %+v", first)
	}

	// A reconnect is not news; a new URL is, once.
	rem.Set(remote.Status{State: remote.StateStarting, Transport: remote.TransportTailscale})
	rem.Set(remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, PublicURL: "https://desk.tail1234.ts.net:8443"})
	rem.Set(remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, PublicURL: "https://studio.tail1234.ts.net:8443"})
	waitMachines(t, rc, 2)
	time.Sleep(50 * time.Millisecond)
	got := rc.machines()
	if len(got) != 2 || got[1].URL != "https://studio.tail1234.ts.net:8443" || got[1].Transport != "tailscale" {
		t.Fatalf("reports = %+v", got)
	}
	for _, m := range got {
		if m.URL == "" {
			t.Fatalf("an empty URL was reported: %+v", got)
		}
	}
}

func TestAccountReporterStartsWithTheTransportBeforeAnyURL(t *testing.T) {
	rem := remote.NewFake(remote.Status{State: remote.StateNeedsLogin, Transport: remote.TransportTailscale})
	a, rc, _ := newTestReporter(t, remote.TransportTailscale, rem)
	saveTestAccount(t, a.dataDir, "me@example.com")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	// The service refuses a machine without a transport, so the very first
	// report must carry it even though there's no URL yet.
	if m := waitMachines(t, rc, 1)[0]; m.Transport != "tailscale" || m.URL != "" {
		t.Fatalf("first report = %+v", m)
	}
}

func TestAccountReporterWithoutAnAccount(t *testing.T) {
	rem := remote.NewFake(remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, PublicURL: "https://desk.tail1234.ts.net:8443"})
	a, rc, logs := newTestReporter(t, remote.TransportTailscale, rem)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	time.Sleep(30 * time.Millisecond)
	if st := a.State(); st.Reporting || len(rc.machines()) != 0 {
		t.Fatalf("reporting without an account: %+v %+v", st, rc.machines())
	}
	// The sign-in is optional: its absence is nothing to warn about.
	if logs.String() != "" {
		t.Fatalf("logged without an account:\n%s", logs.String())
	}

	// Signing in later is picked up without a restart.
	saveTestAccount(t, a.dataDir, "late@example.com")
	if st := a.Reload(); !st.Reporting || st.Email != "late@example.com" {
		t.Fatalf("after reload: %+v", st)
	}
	waitMachines(t, rc, 1)

	// And signing out stops it.
	if err := cloud.DeleteAccount(a.dataDir); err != nil {
		t.Fatal(err)
	}
	if st := a.Reload(); st.Reporting {
		t.Fatalf("still reporting after sign-out: %+v", st)
	}
}

func TestAccountReporterIsQuietForTailscaleWithoutAccountAndOffWithOne(t *testing.T) {
	a, rc, logs := newTestReporter(t, remote.TransportTailscale, remote.NewFake(remote.Status{State: remote.StateRunning}))
	a.Start(context.Background())
	if a.State().Reporting || logs.String() != "" {
		t.Fatalf("tailscale without an account: %+v %q", a.State(), logs.String())
	}

	off, rc2, _ := newTestReporter(t, "", remote.Off{})
	saveTestAccount(t, off.dataDir, "me@example.com")
	off.Start(context.Background())
	time.Sleep(30 * time.Millisecond)
	if off.State().Reporting || len(rc2.machines()) != 0 || len(rc.machines()) != 0 {
		t.Fatal("reported a machine with remote access off")
	}
}

func TestAccountCheck(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cases := []struct {
		transport string
		account   bool
		ok        bool
		info      string
	}{
		{"tailscale", false, true, "optional"},
		{"tailscale", true, true, "signed in as me@example.com"},
		{"off", false, true, ""},
	}
	for _, c := range cases {
		cfg := cfg
		cfg.dataDir = t.TempDir()
		cfg.remoteMode = c.transport
		if c.account {
			saveTestAccount(t, cfg.dataDir, "me@example.com")
		}
		ok, info := accountCheck(cfg)
		if ok != c.ok || (c.info == "" && info != "") || !strings.Contains(info, c.info) {
			t.Errorf("%s account=%v: ok=%v info=%q", c.transport, c.account, ok, info)
		}
	}
}

func TestAccountLoginAsksARunningDaemonToReload(t *testing.T) {
	f, _, out := newAccountFlow(t, "a@example.com\n123456\n", true)
	reloads := 0
	f.reload = func(context.Context) error { reloads++; return nil }
	if err := f.run(context.Background(), "login"); err != nil {
		t.Fatal(err)
	}
	if reloads != 1 || !strings.Contains(out.String(), "now reports this machine's link") || strings.Contains(out.String(), "agentflow restart") {
		t.Fatalf("reloads=%d output:\n%s", reloads, out.String())
	}
	out.Reset()
	if err := f.run(context.Background(), "logout"); err != nil {
		t.Fatal(err)
	}
	if reloads != 2 {
		t.Fatalf("logout didn't tell the daemon: reloads=%d", reloads)
	}
}
