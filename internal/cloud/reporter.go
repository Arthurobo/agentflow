package cloud

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Reporter timing.
const (
	heartbeatInterval = 10 * time.Minute
	minBackoff        = 5 * time.Second
	maxBackoff        = 5 * time.Minute
)

// RunReporter registers the machine and reports every URL change (and a
// periodic heartbeat) until ctx is done, retrying with backoff. It never
// blocks the daemon and never returns an error for a backend outage.
func RunReporter(ctx context.Context, c Client, acct *Account, m Machine, status <-chan URLStatus, log *slog.Logger) {
	r := &reporter{
		client:    c,
		acct:      acct,
		log:       log,
		heartbeat: heartbeatInterval,
		minBack:   minBackoff,
		maxBack:   maxBackoff,
	}
	r.run(ctx, m, status)
}

type reporter struct {
	client  Client
	acct    *Account
	log     *slog.Logger
	onState func(string) // tests observe state changes

	heartbeat, minBack, maxBack time.Duration
}

// latest holds the newest URL status; senders never wait for the reporter.
type latest struct {
	mu     sync.Mutex
	st     URLStatus
	has    bool
	notify chan struct{}
}

func (l *latest) set(st URLStatus) {
	l.mu.Lock()
	l.st, l.has = st, true
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *latest) take() (URLStatus, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.st, l.has
	l.has = false
	return st, ok
}

func (r *reporter) run(ctx context.Context, m Machine, status <-chan URLStatus) {
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	// Drain the transport's channel for as long as ctx lives, even when
	// reporting has stopped, so its sends never block.
	l := &latest{notify: make(chan struct{}, 1)}
	if status != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case st, ok := <-status:
					if !ok {
						return
					}
					l.set(st)
				}
			}
		}()
	}
	if r.acct == nil || r.acct.Token == "" {
		return
	}
	if m.ID == "" {
		m.ID = r.acct.MachineID
	}

	desired := m
	var reported *Machine // last machine the service accepted
	backoff := time.Duration(0)
	failing := false
	next := time.Now() // when to put or heartbeat next

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		r.state("wake")
		select {
		case <-ctx.Done():
			return
		case <-l.notify:
			if st, ok := l.take(); ok {
				if st.Transport != "" {
					desired.Transport = st.Transport
				}
				desired.URL = st.URL
			}
			if reported != nil && *reported == desired {
				continue
			}
			// A change is reported right away, even while backing off.
			next = time.Now()
			backoff = 0
		case <-timer.C:
		}
		if time.Now().Before(next) {
			resetTimer(timer, time.Until(next))
			continue
		}

		var err error
		if reported == nil || *reported != desired {
			err = r.client.PutMachine(ctx, r.acct.Token, desired)
			if err == nil {
				d := desired
				reported = &d
				r.log.Debug("reported machine to the account service", "machine", d.ID, "transport", d.Transport, "url", d.URL)
				r.state("put")
			}
		} else {
			err = r.client.Heartbeat(ctx, r.acct.Token, desired.ID)
			if errors.Is(err, ErrNotFound) {
				// Removed on the dashboard: register again.
				reported = nil
				next = time.Now()
				r.state("reregister")
				resetTimer(timer, 0)
				continue
			}
			if err == nil {
				r.state("heartbeat")
			}
		}
		if ctx.Err() != nil {
			return
		}

		switch {
		case err == nil:
			if failing {
				r.log.Debug("account service reachable again")
			}
			failing, backoff = false, 0
			next = time.Now().Add(r.heartbeat)
		case errors.Is(err, ErrUnauthorized):
			r.log.Warn("the account service rejected this machine's sign-in; link emails are off until `agentflow account login`", "err", err)
			r.stop(ctx, timer)
			return
		case errors.Is(err, ErrNotFound):
			r.log.Warn("the account service refused this machine id; link emails are off", "machine", desired.ID, "err", err)
			r.stop(ctx, timer)
			return
		default:
			if backoff == 0 {
				backoff = r.minBack
			} else {
				backoff = min(backoff*2, r.maxBack)
			}
			if !failing {
				r.log.Warn("couldn't reach the account service; retrying", "err", err, "retryIn", backoff)
			} else {
				r.log.Debug("account service still failing", "err", err, "retryIn", backoff)
			}
			failing = true
			next = time.Now().Add(backoff)
			r.state("retry")
		}
		resetTimer(timer, time.Until(next))
	}
}

// stop ends reporting for good: retrying can't fix a revoked token or a
// machine id that belongs to someone else. It disarms the timer and waits for
// ctx so RunReporter still returns only at shutdown; the drain goroutine keeps
// accepting URL statuses meanwhile.
func (r *reporter) stop(ctx context.Context, timer *time.Timer) {
	timer.Stop()
	r.state("stopped")
	<-ctx.Done()
}

func (r *reporter) state(s string) {
	if r.onState != nil {
		r.onState(s)
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	t.Stop()
	select {
	case <-t.C:
	default:
	}
	t.Reset(max(d, 0))
}
