// Package ratelimit holds the small per-client limiters the daemon puts in
// front of its listeners: a token bucket for request rates and a lockout for
// clients that keep failing authentication.
//
// Both are keyed by client address and forget idle clients on their own, so
// neither needs a goroutine or a Close: every call does an amortized sweep at
// most once per sweepEvery.
package ratelimit

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/remote"
)

// sweepEvery bounds how often a limiter scans for idle clients.
const sweepEvery = time.Minute

// idleAfter is how long a client may be silent before its state is dropped.
// It is longer than every window below, so dropping state never forgives a
// client early.
const idleAfter = time.Hour

// Bucket is a per-key token bucket: rate tokens per second, up to burst.
type Bucket struct {
	mu        sync.Mutex
	rate      float64
	burst     float64
	clients   map[string]*bucketState
	lastSweep time.Time
	now       func() time.Time
}

type bucketState struct {
	tokens float64
	last   time.Time
}

// NewBucket returns a limiter allowing rate requests per second per key with
// the given burst.
func NewBucket(rate float64, burst int) *Bucket {
	return &Bucket{rate: rate, burst: float64(burst), clients: map[string]*bucketState{}, now: time.Now}
}

// Allow spends one token for key and reports whether one was available.
func (b *Bucket) Allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.sweepLocked(now)
	st, ok := b.clients[key]
	if !ok {
		st = &bucketState{tokens: b.burst, last: now}
		b.clients[key] = st
	}
	st.tokens += now.Sub(st.last).Seconds() * b.rate
	if st.tokens > b.burst {
		st.tokens = b.burst
	}
	st.last = now
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	return true
}

// Len reports how many keys the limiter currently tracks.
func (b *Bucket) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

func (b *Bucket) sweepLocked(now time.Time) {
	if now.Sub(b.lastSweep) < sweepEvery {
		return
	}
	b.lastSweep = now
	for k, st := range b.clients {
		if now.Sub(st.last) > idleAfter {
			delete(b.clients, k)
		}
	}
}

// Lockout blocks a key for a while after too many strikes in a window: after
// max strikes within window, the key is blocked for block.
type Lockout struct {
	mu        sync.Mutex
	max       int
	window    time.Duration
	block     time.Duration
	clients   map[string]*lockoutState
	lastSweep time.Time
	now       func() time.Time
}

type lockoutState struct {
	strikes      []time.Time
	blockedUntil time.Time
	last         time.Time
}

// NewLockout returns a lockout of max strikes per window, blocking for block.
func NewLockout(max int, window, block time.Duration) *Lockout {
	return &Lockout{max: max, window: window, block: block, clients: map[string]*lockoutState{}, now: time.Now}
}

// Blocked reports whether key is currently blocked and for how much longer.
func (l *Lockout) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	st, ok := l.clients[key]
	if !ok || !now.Before(st.blockedUntil) {
		return false, 0
	}
	return true, st.blockedUntil.Sub(now)
}

// Strike records one failure for key and blocks it once the window is full.
func (l *Lockout) Strike(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	st, ok := l.clients[key]
	if !ok {
		st = &lockoutState{}
		l.clients[key] = st
	}
	st.last = now
	kept := st.strikes[:0]
	for _, t := range st.strikes {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	st.strikes = append(kept, now)
	if len(st.strikes) >= l.max {
		st.blockedUntil = now.Add(l.block)
		st.strikes = st.strikes[:0]
	}
}

func (l *Lockout) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for k, st := range l.clients {
		if now.Sub(st.last) > idleAfter && !now.Before(st.blockedUntil) {
			delete(l.clients, k)
		}
	}
}

type clientIPKey struct{}

// WithClientIP returns a context carrying the real client address of a
// connection (host or host:port). A listener whose TCP peer is a proxy (the
// Funnel relay) sets it from http.Server.ConnContext.
func WithClientIP(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, addr)
}

// ClientIP returns the address rate limits are keyed on: the address set by
// WithClientIP, else the one the remote package stamps for Funnel
// connections, else the TCP peer. IPv6 clients are keyed by their /64,
// because a single host usually controls a whole /64 and could otherwise
// rotate through it.
func ClientIP(r *http.Request) string {
	addr, _ := r.Context().Value(clientIPKey{}).(string)
	if addr == "" {
		addr = remote.ClientIP(r)
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	ip := net.ParseIP(host)
	if ip == nil {
		if host == "" {
			return "unknown"
		}
		return host
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}
