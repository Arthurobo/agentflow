package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/remote"
	"github.com/arthurobo/agentflow/internal/store"
)

var phoneRequest = adminsock.PairRequest{
	ID: "0f3a9c1e5b7d4a2f8e6c0b1d3f5a7c9e", Name: "iPhone · Safari", MatchCode: "4821",
	ClientIP: "203.0.113.7", CreatedAt: time.Now().Add(-2 * time.Minute).UnixMilli(),
}

func TestApproveByMatchCodeThroughTheDaemon(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	d.addRequest(phoneRequest)
	var out bytes.Buffer
	if err := decideAccessRequest(context.Background(), &out, adminClient(cfg), "4821", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Approved iPhone · Safari (code 4821)") {
		t.Fatalf("output: %s", out.String())
	}
	if got := d.decided(); len(got) != 1 || got[0] != "approve "+phoneRequest.ID {
		t.Fatalf("decisions = %v", got)
	}
	err := decideAccessRequest(context.Background(), &out, adminClient(cfg), "4821", false)
	if err == nil || !strings.Contains(err.Error(), "no pending access request \"4821\"") {
		t.Fatalf("deciding a request twice: %v", err)
	}
}

func TestDecideWithoutADaemonSaysWhy(t *testing.T) {
	cfg := testConfig(shortDataDir(t))
	err := decideAccessRequest(context.Background(), io.Discard, adminClient(cfg), "4821", true)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v", err)
	}
}

func TestDevicesListsPendingAccessRequests(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	var out bytes.Buffer
	// No daemon: nothing to list, and that's not an error.
	if err := listAccessRequests(context.Background(), &out, adminClient(cfg), time.Now()); err != nil || out.Len() != 0 {
		t.Fatalf("without a daemon: %q %v", out.String(), err)
	}
	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	if err := listAccessRequests(context.Background(), &out, adminClient(cfg), time.Now()); err != nil || out.Len() != 0 {
		t.Fatalf("with none pending: %q %v", out.String(), err)
	}
	d.addRequest(phoneRequest)
	if err := listAccessRequests(context.Background(), &out, adminClient(cfg), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agentflow approve CODE", "4821", "iPhone · Safari", "203.0.113.7", "2 min ago", phoneRequest.ID} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestWatchAsksAboutEachNewRequest(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	d.addRequest(phoneRequest)
	second := adminsock.PairRequest{ID: "req-2", Name: "Pixel · Chrome", MatchCode: "1234"}
	third := adminsock.PairRequest{ID: "req-3", Name: "iPad · Safari", MatchCode: "7777"}

	inR, inW := io.Pipe()
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watchAccessRequests(ctx, inR, out, adminClient(cfg), 5*time.Millisecond) }()

	waitFor := func(text string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.String(), text) {
			if time.Now().After(deadline) {
				t.Fatalf("output never showed %q:\n%s", text, out.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("iPhone · Safari (code 4821) wants access — approve? [y/N]")
	_, _ = io.WriteString(inW, "y\n")
	waitFor("Approved iPhone · Safari")

	d.addRequest(second)
	waitFor("Pixel · Chrome (code 1234) wants access")
	_, _ = io.WriteString(inW, "\n") // the default is no
	waitFor("Denied Pixel · Chrome")

	d.addRequest(third)
	waitFor("iPad · Safari (code 7777) wants access")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("watch returned %v", err)
	}
	if got := d.decided(); strings.Join(got, ",") != "approve "+phoneRequest.ID+",deny req-2" {
		t.Fatalf("decisions = %v", got)
	}
	if strings.Count(out.String(), "iPhone · Safari (code 4821) wants access") != 1 {
		t.Fatalf("asked about the same request twice:\n%s", out.String())
	}
}

func TestWatchStopsWhenInputEnds(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	d.addRequest(phoneRequest)
	err := watchAccessRequests(context.Background(), strings.NewReader(""), io.Discard, adminClient(cfg), 5*time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := d.decided(); len(got) != 0 {
		t.Fatalf("decided with no answer: %v", got)
	}
}

func TestStartWatchesForAccessRequestsAfterPairing(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	cfg.remoteMode = "off"
	startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	out := &syncBuffer{}
	f, _, _ := newTestStartFlow(t, cfg, out)
	watched := false
	f.watch = func(context.Context) error {
		watched = true
		if !strings.Contains(out.String(), "/pair/#token=tok123") {
			t.Error("watching started before the pairing link was shown")
		}
		return nil
	}
	if err := f.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !watched || !strings.Contains(out.String(), "Watching for access requests") {
		t.Fatalf("watched=%v output:\n%s", watched, out.String())
	}
}

// The whole path a phone's request takes: asked on the public handler,
// approved with `agentflow approve CODE` through the admin socket of the real
// API, collected by the phone's poll.
func TestAccessRequestApprovedFromTheCLIReachesThePhone(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	api := agentapi.New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg.machineID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := adminsock.Start(ctx, adminsock.Config{
		Dir:               adminsock.Dir(dataDir),
		Status:            func() any { return remote.Off{}.Status() },
		Logout:            func(context.Context) error { return nil },
		Revoke:            func(context.Context, string) (int, error) { return 0, nil },
		MintPair:          api.MintPairingToken,
		Health:            func() any { return map[string]any{} },
		PairRequests:      adminPairRequests(api),
		DecidePairRequest: adminDecidePairRequest(api),
		ReloadAccount:     func() any { return accountState{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	phone := api.PublicHandler()
	rr := httptest.NewRecorder()
	phone.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/agentd/pair/request", strings.NewReader(`{"name":"iPhone · Safari"}`)))
	var created struct {
		RequestID, PollSecret, MatchCode string
	}
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &created) != nil {
		t.Fatalf("request: %d %s", rr.Code, rr.Body.String())
	}

	var listed bytes.Buffer
	if err := listAccessRequests(ctx, &listed, adminClient(cfg), time.Now()); err != nil || !strings.Contains(listed.String(), created.MatchCode) {
		t.Fatalf("devices listing: %v\n%s", err, listed.String())
	}
	var out bytes.Buffer
	if err := decideAccessRequest(ctx, &out, adminClient(cfg), created.MatchCode, true); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/v1/agentd/pair/request/"+created.RequestID, nil)
	req.Header.Set(agentapi.PairRequestPollHeader, created.PollSecret)
	rr = httptest.NewRecorder()
	phone.ServeHTTP(rr, req)
	var polled struct {
		Status, DeviceToken string
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &polled); err != nil || polled.Status != "approved" || polled.DeviceToken == "" {
		t.Fatalf("poll: %d %s", rr.Code, rr.Body.String())
	}
	if dev, err := st.VerifyClientDevice(ctx, polled.DeviceToken); err != nil || dev == nil {
		t.Fatalf("token does not authenticate: %v", err)
	}
}

func TestDecideEscapesOddTargets(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	d.addRequest(phoneRequest)
	for _, target := range []string{"../../devices/dev-1", "4821/approve", "a?b#c", "%2e%2e"} {
		err := decideAccessRequest(context.Background(), io.Discard, adminClient(cfg), target, true)
		if err == nil || !strings.Contains(err.Error(), "no pending access request") {
			t.Errorf("target %q: %v", target, err)
		}
	}
	if got := d.decided(); len(got) != 0 {
		t.Fatalf("an odd target decided something: %v", got)
	}
}
