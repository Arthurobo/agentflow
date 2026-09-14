package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Fake is a Transport for tests. Run serves the handler on a local httptest
// listener (with the same client-IP ConnContext as the real listener), and
// the test moves it through states with Set or Script. It never touches the
// network beyond loopback.
type Fake struct {
	bus   *statusBus
	ready chan struct{}

	mu        sync.Mutex
	srv       *httptest.Server
	logouts   int
	logoutErr error
}

// NewFake returns a Fake whose status starts as initial (transport defaults
// to tailscale and mode to funnel, unless it starts off).
func NewFake(initial Status) *Fake {
	if initial.State == "" {
		initial.State = StateStarting
	}
	if initial.State != StateOff {
		if initial.Transport == "" {
			initial.Transport = TransportTailscale
		}
		if initial.Mode == "" && initial.Transport == TransportTailscale {
			initial.Mode = ModeFunnel
		}
	}
	return &Fake{bus: newStatusBus(initial, nil), ready: make(chan struct{})}
}

// Run serves h until ctx is done.
func (f *Fake) Run(ctx context.Context, h http.Handler) error {
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnContext = connContext
	srv.Start()
	f.mu.Lock()
	f.srv = srv
	f.mu.Unlock()
	close(f.ready)
	<-ctx.Done()
	srv.Close()
	return nil
}

// URL waits (up to timeout) for Run to start serving and returns the
// listener's base URL.
func (f *Fake) URL(timeout time.Duration) (string, error) {
	select {
	case <-f.ready:
	case <-time.After(timeout):
		return "", errors.New("fake remote: Run has not started")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.srv.URL, nil
}

// Set publishes st. A running status without a PublicURL gets the listener's
// URL, so tests can reach the served handler through it.
func (f *Fake) Set(st Status) {
	if prev := f.bus.snapshot(); st.State != StateOff {
		if st.Transport == "" {
			st.Transport = prev.Transport
		}
		if st.Mode == "" {
			st.Mode = prev.Mode
		}
	}
	if st.State == StateRunning && st.PublicURL == "" {
		f.mu.Lock()
		if f.srv != nil {
			st.PublicURL = f.srv.URL
		}
		f.mu.Unlock()
	}
	st.UpdatedAt = time.Now()
	f.bus.publish(st)
}

// Script publishes steps in order, gap apart, in the background.
func (f *Fake) Script(ctx context.Context, gap time.Duration, steps ...Status) {
	go func() {
		for _, st := range steps {
			select {
			case <-ctx.Done():
				return
			case <-time.After(gap):
			}
			f.Set(st)
		}
	}()
}

// FailLogout makes the next Logout calls return err.
func (f *Fake) FailLogout(err error) {
	f.mu.Lock()
	f.logoutErr = err
	f.mu.Unlock()
}

// Logouts returns how many times Logout succeeded.
func (f *Fake) Logouts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logouts
}

func (f *Fake) Status() Status                     { return f.bus.snapshot() }
func (f *Fake) Subscribe() (<-chan Status, func()) { return f.bus.subscribe() }

// Logout records the call and moves to needs_login, like the real remote.
func (f *Fake) Logout(context.Context) error {
	f.mu.Lock()
	err := f.logoutErr
	if err == nil {
		f.logouts++
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.Set(Status{State: StateNeedsLogin, AuthURL: "https://login.tailscale.com/a/fake"})
	return nil
}

func (f *Fake) Close() error { f.bus.close(); return nil }
