package cloud

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedClient is a Client whose PutMachine and Heartbeat results the test
// controls.
type scriptedClient struct {
	mu         sync.Mutex
	puts       []Machine
	heartbeats int
	putErrs    []error // consumed in order; nil entries succeed
	hbErrs     []error
	putGate    chan struct{} // when set, PutMachine waits for a value
	gated      chan struct{} // receives when a PutMachine starts waiting on putGate
	calls      chan string
}

func newScripted() *scriptedClient { return &scriptedClient{calls: make(chan string, 100)} }

func (c *scriptedClient) RequestCode(context.Context, string) error { return nil }
func (c *scriptedClient) Verify(context.Context, string, string) (string, error) {
	return "", nil
}
func (c *scriptedClient) Logout(context.Context, string) error { return nil }

func (c *scriptedClient) PutMachine(ctx context.Context, token string, m Machine) error {
	c.mu.Lock()
	gate := c.putGate
	c.mu.Unlock()
	if gate != nil {
		if c.gated != nil {
			c.gated <- struct{}{}
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if len(c.putErrs) > 0 {
		err, c.putErrs = c.putErrs[0], c.putErrs[1:]
	}
	if err == nil {
		c.puts = append(c.puts, m)
	}
	c.calls <- "put"
	return err
}

func (c *scriptedClient) Heartbeat(ctx context.Context, token, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if len(c.hbErrs) > 0 {
		err, c.hbErrs = c.hbErrs[0], c.hbErrs[1:]
	}
	if err == nil {
		c.heartbeats++
	}
	c.calls <- "heartbeat"
	return err
}

func (c *scriptedClient) snapshot() ([]Machine, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Machine(nil), c.puts...), c.heartbeats
}

// waitCall waits for the next client call and returns its kind.
func (c *scriptedClient) waitCall(t *testing.T) string {
	t.Helper()
	select {
	case k := <-c.calls:
		return k
	case <-time.After(5 * time.Second):
		t.Fatal("no call to the account service")
		return ""
	}
}

func (c *scriptedClient) noCall(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case k := <-c.calls:
		t.Fatalf("unexpected %s", k)
	case <-time.After(d):
	}
}

var testAcct = &Account{Email: "a@example.com", Token: "tok", MachineID: "0123456789abcdef0123456789abcdef"}

func startReporter(t *testing.T, c Client, acct *Account, m Machine, status <-chan URLStatus, heartbeat time.Duration) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &reporter{client: c, acct: acct, heartbeat: heartbeat, minBack: 10 * time.Millisecond, maxBack: 40 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		r.run(ctx, m, status)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

func TestReporterPutsOnStartAndOnURLChange(t *testing.T) {
	c := newScripted()
	status := make(chan URLStatus)
	startReporter(t, c, testAcct, Machine{Name: "box", Transport: "tailscale", Version: "1.4.0"}, status, time.Hour)

	if k := c.waitCall(t); k != "put" {
		t.Fatalf("first call %s, want put", k)
	}
	status <- URLStatus{Transport: "tailscale", URL: "https://box.tail1234.ts.net:8443"}
	if k := c.waitCall(t); k != "put" {
		t.Fatalf("after URL: %s", k)
	}
	// The same URL again isn't reported.
	status <- URLStatus{Transport: "tailscale", URL: "https://box.tail1234.ts.net:8443"}
	c.noCall(t, 100*time.Millisecond)

	status <- URLStatus{Transport: "tailscale", URL: "https://desk.tail1234.ts.net:8443"}
	if k := c.waitCall(t); k != "put" {
		t.Fatalf("after transport change: %s", k)
	}
	status <- URLStatus{Transport: "tailscale", URL: ""}
	if k := c.waitCall(t); k != "put" {
		t.Fatalf("after going offline: %s", k)
	}

	puts, _ := c.snapshot()
	want := []Machine{
		{ID: testAcct.MachineID, Name: "box", Transport: "tailscale", Version: "1.4.0"},
		{ID: testAcct.MachineID, Name: "box", Transport: "tailscale", URL: "https://box.tail1234.ts.net:8443", Version: "1.4.0"},
		{ID: testAcct.MachineID, Name: "box", Transport: "tailscale", URL: "https://desk.tail1234.ts.net:8443", Version: "1.4.0"},
		{ID: testAcct.MachineID, Name: "box", Transport: "tailscale", URL: "", Version: "1.4.0"},
	}
	if len(puts) != len(want) {
		t.Fatalf("puts = %+v", puts)
	}
	for i := range want {
		if puts[i] != want[i] {
			t.Errorf("put %d = %+v, want %+v", i, puts[i], want[i])
		}
	}
}

func TestReporterHeartbeats(t *testing.T) {
	c := newScripted()
	startReporter(t, c, testAcct, Machine{ID: "m1", Name: "box", Transport: "tailscale"}, nil, 30*time.Millisecond)
	if k := c.waitCall(t); k != "put" {
		t.Fatal(k)
	}
	for i := range 3 {
		if k := c.waitCall(t); k != "heartbeat" {
			t.Fatalf("call %d = %s, want heartbeat", i, k)
		}
	}
	puts, _ := c.snapshot()
	if len(puts) != 1 || puts[0].ID != "m1" {
		t.Fatalf("puts = %+v (an explicit machine id wins over the account's)", puts)
	}
}

func TestReporterRetriesWithBackoff(t *testing.T) {
	c := newScripted()
	outage := errors.New("connection refused")
	c.putErrs = []error{outage, outage, &APIError{Status: 503}, nil}
	start := time.Now()
	startReporter(t, c, testAcct, Machine{Name: "box", Transport: "tailscale"}, nil, time.Hour)
	for range 4 {
		if k := c.waitCall(t); k != "put" {
			t.Fatal(k)
		}
	}
	// Backoff 10ms, 20ms, 40ms before the fourth attempt.
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("four attempts in %s; retries aren't backing off", elapsed)
	}
	if puts, _ := c.snapshot(); len(puts) != 1 {
		t.Fatalf("puts = %+v", puts)
	}
	c.noCall(t, 100*time.Millisecond)
}

func TestReporterURLChangeSkipsBackoff(t *testing.T) {
	c := newScripted()
	outage := errors.New("connection refused")
	c.putErrs = []error{outage}
	status := make(chan URLStatus)
	ctx, cancel := context.WithCancel(context.Background())
	r := &reporter{client: c, acct: testAcct, heartbeat: time.Hour, minBack: time.Hour, maxBack: time.Hour}
	done := make(chan struct{})
	go func() { r.run(ctx, Machine{Name: "box", Transport: "tailscale"}, status); close(done) }()
	defer func() { cancel(); <-done }()

	if k := c.waitCall(t); k != "put" {
		t.Fatal(k)
	}
	status <- URLStatus{Transport: "tailscale", URL: "https://new.tail1234.ts.net"}
	if k := c.waitCall(t); k != "put" {
		t.Fatalf("a new URL should be reported without waiting out the backoff: %s", k)
	}
}

func TestReporterStopsOnRejectedTokenButKeepsDraining(t *testing.T) {
	c := newScripted()
	c.putErrs = []error{&APIError{Status: 401, Code: "unauthorized"}}
	status := make(chan URLStatus) // unbuffered: a stalled reporter would block the sender
	startReporter(t, c, testAcct, Machine{Name: "box", Transport: "tailscale"}, status, 10*time.Millisecond)
	if k := c.waitCall(t); k != "put" {
		t.Fatal(k)
	}
	sent := make(chan struct{})
	go func() {
		for i := range 50 {
			status <- URLStatus{Transport: "tailscale", URL: "https://x" + string(rune('a'+i%26)) + ".tail1234.ts.net"}
		}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("status sends blocked after reporting stopped")
	}
	c.noCall(t, 100*time.Millisecond)
}

func TestReporterMachineIDTakenStops(t *testing.T) {
	c := newScripted()
	c.putErrs = []error{&APIError{Status: 404, Code: "not_found"}}
	startReporter(t, c, testAcct, Machine{Name: "box", Transport: "tailscale"}, nil, 10*time.Millisecond)
	if k := c.waitCall(t); k != "put" {
		t.Fatal(k)
	}
	c.noCall(t, 100*time.Millisecond)
}

func TestReporterReregistersRemovedMachine(t *testing.T) {
	c := newScripted()
	c.hbErrs = []error{&APIError{Status: 404, Code: "not_found"}}
	startReporter(t, c, testAcct, Machine{Name: "box", Transport: "tailscale", URL: "https://box.tail1234.ts.net:8443"}, nil, 20*time.Millisecond)
	want := []string{"put", "heartbeat", "put", "heartbeat"}
	for i, w := range want {
		if k := c.waitCall(t); k != w {
			t.Fatalf("call %d = %s, want %s", i, k, w)
		}
	}
	if puts, _ := c.snapshot(); len(puts) != 2 || puts[1].URL != "https://box.tail1234.ts.net:8443" {
		t.Fatalf("puts = %+v", puts)
	}
}

func TestReporterLatestURLWinsWhileBusy(t *testing.T) {
	c := newScripted()
	gate := make(chan struct{})
	c.putGate = gate
	c.gated = make(chan struct{}, 4)
	status := make(chan URLStatus)
	startReporter(t, c, testAcct, Machine{Name: "box", Transport: "tailscale"}, status, time.Hour)
	select {
	case <-c.gated:
	case <-time.After(5 * time.Second):
		t.Fatal("the start put never ran")
	}

	// The first put is stuck; sends must still go through.
	sent := make(chan struct{})
	go func() {
		status <- URLStatus{Transport: "tailscale", URL: "https://one.tail1234.ts.net"}
		status <- URLStatus{Transport: "tailscale", URL: "https://two.tail1234.ts.net"}
		status <- URLStatus{Transport: "tailscale", URL: "https://three.tail1234.ts.net"}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("sends blocked while the service was slow")
	}
	close(gate)
	// A send completes when the reporter receives it, a moment before it is
	// recorded, so "two" may be reported before "three" catches up. What
	// must hold: the stale "one" is never reported and the last report is
	// "three".
	deadline := time.Now().Add(5 * time.Second)
	for {
		puts, _ := c.snapshot()
		if n := len(puts); n > 1 && puts[n-1].URL == "https://three.tail1234.ts.net" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("puts = %+v, want the latest URL reported", puts)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for len(c.calls) > 0 {
		<-c.calls
	}
	c.noCall(t, 100*time.Millisecond)
	puts, _ := c.snapshot()
	if len(puts) > 3 {
		t.Fatalf("puts = %+v, want at most the start put, one intermediate and the latest", puts)
	}
	for _, p := range puts {
		if p.URL == "https://one.tail1234.ts.net" {
			t.Fatalf("puts = %+v: a URL superseded while the service was busy was still reported", puts)
		}
	}
}

func TestReporterWithoutAccountOnlyDrains(t *testing.T) {
	c := newScripted()
	status := make(chan URLStatus)
	ctx, cancel := context.WithCancel(context.Background())
	RunReporter(ctx, c, nil, Machine{Name: "box"}, status, nil)
	sent := make(chan struct{})
	go func() {
		status <- URLStatus{URL: "https://one.tail1234.ts.net"}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked without an account")
	}
	cancel()
	c.noCall(t, 50*time.Millisecond)
}

func TestRunReporterAgainstFakeService(t *testing.T) {
	f := newFakeService(t)
	f.mu.Lock()
	f.tokens["tok"] = "a@example.com"
	f.failNext = 1
	f.mu.Unlock()
	c := NewClient(f.srv.URL)
	status := make(chan URLStatus, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &reporter{client: c, acct: testAcct, heartbeat: 20 * time.Millisecond, minBack: 10 * time.Millisecond, maxBack: 20 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		r.run(ctx, Machine{Name: "box", Transport: "tailscale", Version: "1.4.0"}, status)
		close(done)
	}()
	status <- URLStatus{Transport: "tailscale", URL: "https://desk.tail1234.ts.net:8443"}

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		m := f.machines[testAcct.MachineID]
		hb := f.heartbeats
		f.mu.Unlock()
		if m["url"] == "https://desk.tail1234.ts.net:8443" && hb > 0 {
			if m["transport"] != "tailscale" || m["name"] != "box" || m["agentflowVersion"] != "1.4.0" {
				t.Fatalf("stored machine = %v", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine = %v, heartbeats = %d", m, hb)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestReporterStopsWithoutSpinningAfterLateRejection(t *testing.T) {
	for _, rejection := range []error{
		&APIError{Status: 401, Code: "unauthorized"},
		&APIError{Status: 404, Code: "not_found"},
	} {
		t.Run(rejection.Error(), func(t *testing.T) {
			c := newScripted()
			// The start put succeeds and arms the heartbeat timer; the put
			// for the changed URL is rejected.
			c.putErrs = []error{nil, rejection}
			var wakes atomic.Int64
			status := make(chan URLStatus)
			ctx, cancel := context.WithCancel(context.Background())
			r := &reporter{client: c, acct: testAcct, heartbeat: 20 * time.Millisecond, minBack: 10 * time.Millisecond, maxBack: 20 * time.Millisecond,
				onState: func(s string) {
					if s == "wake" {
						wakes.Add(1)
					}
				}}
			done := make(chan struct{})
			go func() { r.run(ctx, Machine{Name: "box", Transport: "tailscale"}, status); close(done) }()
			defer func() { cancel(); <-done }()

			if k := c.waitCall(t); k != "put" {
				t.Fatal(k)
			}
			status <- URLStatus{Transport: "tailscale", URL: "https://new.tail1234.ts.net"}
			if k := c.waitCall(t); k != "put" {
				t.Fatal(k)
			}
			before := wakes.Load()
			// Several heartbeat intervals: the stopped reporter must stay idle.
			c.noCall(t, 200*time.Millisecond)
			if n := wakes.Load() - before; n > 3 {
				t.Fatalf("stopped reporter woke %d times in 200ms; it is spinning", n)
			}
			// Sends are still drained.
			select {
			case status <- URLStatus{Transport: "tailscale", URL: "https://later.tail1234.ts.net"}:
			case <-time.After(5 * time.Second):
				t.Fatal("send blocked after reporting stopped")
			}
		})
	}
}
