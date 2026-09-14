package ratelimit

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestBucketRefillsAtItsRate(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := NewBucket(5.0/60.0, 5)
	b.now = c.now
	for i := 0; i < 5; i++ {
		if !b.Allow("ip") {
			t.Fatalf("attempt %d within burst refused", i+1)
		}
	}
	if b.Allow("ip") {
		t.Fatal("sixth attempt in the same instant allowed")
	}
	c.t = c.t.Add(12 * time.Second)
	if !b.Allow("ip") || b.Allow("ip") {
		t.Fatal("12s at 5/min must refill exactly one token")
	}
}

func TestBucketForgetsIdleClients(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := NewBucket(1, 1)
	b.now = c.now
	b.Allow("a")
	b.Allow("b")
	c.t = c.t.Add(2 * time.Hour)
	b.Allow("c")
	if n := b.Len(); n != 1 {
		t.Fatalf("idle clients kept: %d entries", n)
	}
}

func TestLockoutBlocksForItsDurationAfterTheWindowFills(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := NewLockout(20, 10*time.Minute, 15*time.Minute)
	l.now = c.now
	for i := 0; i < 19; i++ {
		l.Strike("ip")
		c.t = c.t.Add(time.Second)
	}
	if blocked, _ := l.Blocked("ip"); blocked {
		t.Fatal("blocked before the limit")
	}
	l.Strike("ip")
	if blocked, left := l.Blocked("ip"); !blocked || left != 15*time.Minute {
		t.Fatalf("after 20 strikes: blocked=%v left=%v", blocked, left)
	}
	c.t = c.t.Add(15 * time.Minute)
	if blocked, _ := l.Blocked("ip"); blocked {
		t.Fatal("still blocked after the block expired")
	}
}

func TestLockoutStrikesOutsideTheWindowDoNotCount(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := NewLockout(20, 10*time.Minute, 15*time.Minute)
	l.now = c.now
	for i := 0; i < 40; i++ {
		l.Strike("ip")
		c.t = c.t.Add(time.Minute) // 1/min never reaches 20 in 10 minutes
	}
	if blocked, _ := l.Blocked("ip"); blocked {
		t.Fatal("slow strikes spread over hours blocked the client")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.1:4000"
	if got := ClientIP(r); got != "192.0.2.1" {
		t.Fatalf("tcp peer: %q", got)
	}
	r = r.WithContext(WithClientIP(context.Background(), "203.0.113.9:443"))
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("stamped client: %q", got)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[2001:db8:1:2:3:4:5:6]:4000"
	if got := ClientIP(r); got != "2001:db8:1:2::/64" {
		t.Fatalf("ipv6: %q", got)
	}
}
