package cloudflared

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// RunArgs runs the named tunnel whose token is in the TUNNEL_TOKEN
// environment variable, with metrics (and /ready) on a free loopback port that
// cloudflared names in its log. Auto-update stays off: agentflow pins and
// verifies the binary it downloads, and a package-managed one is updated by
// its manager. Ingress comes from the tunnel's configuration at Cloudflare.
func RunArgs() []string {
	return []string{"tunnel", "--no-autoupdate", "--metrics", "127.0.0.1:0", "run"}
}

// TokenEnv is the environment entry that hands cloudflared the tunnel token.
func TokenEnv(token string) string { return "TUNNEL_TOKEN=" + token }

var (
	// "INF Starting metrics server on 127.0.0.1:41235/metrics"
	metricsLine = regexp.MustCompile(`Starting metrics server on ((?:127\.0\.0\.1|\[::1\]|localhost):\d{1,5})/metrics`)
	// "INF Registered tunnel connection connIndex=0 connection=... location=lhr01 protocol=quic"
	registeredLine   = regexp.MustCompile(`Registered tunnel connection connIndex=(\d+)`)
	unregisteredLine = regexp.MustCompile(`(?:Unregistered tunnel connection|Connection terminated|Retrying connection in up to).*connIndex=(\d+)`)
)

// Readiness follows one cloudflared process's log: where its metrics
// listener is, and which edge connections are registered.
type Readiness struct {
	mu          sync.Mutex
	metricsAddr string
	conns       map[string]bool
}

// Line records what a log line says.
func (r *Readiness) Line(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := metricsLine.FindStringSubmatch(line); m != nil {
		r.metricsAddr = m[1]
	}
	if m := registeredLine.FindStringSubmatch(line); m != nil {
		if r.conns == nil {
			r.conns = map[string]bool{}
		}
		r.conns[m[1]] = true
	} else if m := unregisteredLine.FindStringSubmatch(line); m != nil {
		delete(r.conns, m[1])
	}
}

// Reset forgets everything, for a new process.
func (r *Readiness) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metricsAddr, r.conns = "", nil
}

func (r *Readiness) snapshot() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metricsAddr, len(r.conns)
}

// Ready reports whether the tunnel has a live connection to Cloudflare's
// edge. The metrics listener's /ready (200 with at least one connection, 503
// without) is the authority; the log is the fallback when cloudflared hasn't
// named the listener or it doesn't answer.
func (r *Readiness) Ready(ctx context.Context, client *http.Client) bool {
	addr, conns := r.snapshot()
	if addr != "" {
		if ok, err := readyEndpoint(ctx, client, addr); err == nil {
			return ok
		}
	}
	return conns > 0
}

func readyEndpoint(ctx context.Context, client *http.Client, addr string) (bool, error) {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/ready", nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusServiceUnavailable:
		return false, nil
	}
	return false, errUnexpectedReady
}

type readyError string

func (e readyError) Error() string { return string(e) }

const errUnexpectedReady = readyError("cloudflared /ready: unexpected status")
