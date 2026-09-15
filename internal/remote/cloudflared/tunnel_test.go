package cloudflared

import (
	"context"
	"strings"
	"testing"
)

func TestRunArgsKeepTheTokenOut(t *testing.T) {
	args := strings.Join(RunArgs(), " ")
	if args != "tunnel --no-autoupdate --metrics 127.0.0.1:0 run" {
		t.Fatalf("args = %s", args)
	}
	if TokenEnv("abc") != "TUNNEL_TOKEN=abc" {
		t.Fatal(TokenEnv("abc"))
	}
}

func TestReadinessFromLog(t *testing.T) {
	r := &Readiness{}
	ctx := context.Background()
	if r.Ready(ctx, nil) {
		t.Fatal("ready with no log")
	}
	for _, l := range []string{
		"2026-09-15T10:00:00Z INF Starting tunnel tunnelID=5f7f",
		"2026-09-15T10:00:00Z INF Starting metrics server on 10.0.0.5:2000/metrics",
		"2026-09-15T10:00:01Z INF Registered tunnel connection connIndex=0 connection=a location=lhr01 protocol=quic",
		"2026-09-15T10:00:01Z INF Registered tunnel connection connIndex=1 connection=b location=man01 protocol=quic",
	} {
		r.Line(l)
	}
	if addr, conns := r.snapshot(); addr != "" || conns != 2 {
		t.Fatalf("addr %q (a non-loopback metrics address must be ignored), conns %d", addr, conns)
	}
	if !r.Ready(ctx, nil) {
		t.Fatal("not ready with two registered connections")
	}
	r.Line("2026-09-15T10:05:00Z WRN Connection terminated error=\"timeout\" connIndex=0")
	r.Line("2026-09-15T10:05:00Z INF Unregistered tunnel connection connIndex=1 event=0")
	if r.Ready(ctx, nil) {
		t.Fatal("ready after every connection went away")
	}
	r.Line("2026-09-15T10:05:02Z INF Registered tunnel connection connIndex=0 connection=c location=lhr01 protocol=quic")
	if !r.Ready(ctx, nil) {
		t.Fatal("not ready after reconnecting")
	}
	r.Line("INF Starting metrics server on 127.0.0.1:20241/metrics")
	if addr, _ := r.snapshot(); addr != "127.0.0.1:20241" {
		t.Fatalf("metrics addr = %q", addr)
	}
	r.Reset()
	if addr, conns := r.snapshot(); addr != "" || conns != 0 {
		t.Fatal("reset kept state")
	}
}
