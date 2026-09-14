package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// courier delivers loop mail INTO a member's terminal.
//
// The model it replaces: every member ran a blocking foreground poll
// (`while true; do curl ...inbox?wait=N; done`) and the brief insisted it stay
// in the foreground, because a backgrounded wait swallows messages into a job
// nobody reads. Both halves of that trade are bad. A member blocked in the
// foreground poll is DEAF: it cannot see mail arriving by any other route and
// it cannot see the engineer typing into its terminal. That is not a theory —
// a live loop froze exactly this way, with a typed prompt sitting queued
// behind a curl that had 300 seconds left to run.
//
// So agentd delivers instead. When mail lands for a member whose process this
// host owns, the courier claims it and submits the body as a prompt, the same
// path the composer uses. The member sits at its prompt doing nothing, which
// is now the correct thing to do, and it hears mail and a human identically.
//
// It is an accelerator over a bus that still works without it, exactly like
// the nudger: a member running somewhere agentd did not spawn it still has
// the inbox endpoint, and every guarantee — claim, lease, redelivery, ack —
// is the store's and is unchanged.
type courier struct {
	st      *store.Store
	submit  func(runID, text string) error
	sp      *spawner.Spawner
	engines *engine.Registry
	log     *slog.Logger
	// budget reports where a loop stands against its limits; nil in tests
	// that do not care. A loop over a limit gets nothing more written into
	// its members' terminals, and the sweeper ends it.
	budget func(ctx context.Context, loopID string) (store.BudgetStatus, error)
	// writeTimeout bounds one write into a member's terminal.
	writeTimeout time.Duration

	// slots bounds how many members are being written to at once.
	slots chan struct{}

	mu sync.Mutex
	// members is the delivery state per member id: whether a delivery is in
	// flight, and the backoff after failed writes.
	members map[string]*memberDelivery
}

// memberDelivery is what the courier remembers about one member.
type memberDelivery struct {
	// busy is set while a delivery runs, and stays set while a write that
	// timed out is still stuck in the terminal: nothing more is written
	// into a terminal that has not taken the last write.
	busy bool
	// failures is how many deliveries in a row have failed; next is when
	// the next may be tried.
	failures int
	next     time.Time
	// seen is the last time anything was scheduled for this member, for
	// forgetting members that are long gone.
	seen time.Time
}

// courierSweep is the safety pass over the fast path. The watch channel
// carries a member id the instant mail commits; this catches whatever wakes
// nothing (the sweep's own stranded announcement writes its row directly) and
// anything that arrived while a delivery was in flight.
const courierSweep = 5 * time.Second

// courierMemory is how long delivery state is kept for a member nothing has
// been scheduled for.
const courierMemory = 2 * time.Hour

// courierBatch is how many messages are handed over in one go. A member that
// has been away can have a queue; delivering all of it at once would paste an
// unbounded amount of text into one prompt.
const courierBatch = 5

// courierWorkers is how many members are delivered to at the same time. One
// terminal that stops reading must not hold up every other member's mail.
const courierWorkers = 4

// courierWriteTimeout bounds a single write into a member's terminal. A PTY
// whose reader has stalled blocks the writer indefinitely.
const courierWriteTimeout = 15 * time.Second

// Backoff after a failed delivery: doubling from the base, capped.
const (
	courierBackoffBase = 5 * time.Second
	courierBackoffMax  = 5 * time.Minute
)

func newCourier(st *store.Store, submit func(runID, text string) error, sp *spawner.Spawner,
	engines *engine.Registry, log *slog.Logger) *courier {
	return &courier{
		st:           st,
		submit:       submit,
		sp:           sp,
		engines:      engines,
		log:          log,
		budget:       st.Budget,
		writeTimeout: courierWriteTimeout,
		slots:        make(chan struct{}, courierWorkers),
		members:      map[string]*memberDelivery{},
	}
}

// backoff is the wait after the given number of consecutive failures.
func backoff(failures int) time.Duration {
	d := courierBackoffBase
	for i := 1; i < failures && d < courierBackoffMax; i++ {
		d *= 2
	}
	if d > courierBackoffMax {
		d = courierBackoffMax
	}
	return d
}

// claimMember marks a member busy if it is free and not backing off.
func (c *courier) claimMember(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	st, ok := c.members[id]
	if !ok {
		st = &memberDelivery{}
		c.members[id] = st
	}
	st.seen = now
	if st.busy || now.Before(st.next) {
		return false
	}
	st.busy = true
	return true
}

// releaseMember records how a delivery went and frees the member.
func (c *courier) releaseMember(id string, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.members[id]
	if !ok {
		return
	}
	st.busy = false
	if failed {
		st.failures++
		st.next = time.Now().Add(backoff(st.failures))
		return
	}
	st.failures, st.next = 0, time.Time{}
}

// forget drops delivery state for members nothing has touched in a long time.
func (c *courier) forget(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range c.members {
		if !st.busy && now.Sub(st.seen) > courierMemory {
			delete(c.members, id)
		}
	}
}

// deliverTo hands one member whatever is waiting for it, in order, and
// returns how many messages reached its terminal.
//
// Claim first, then write. Claiming is what stops two deliverers racing for
// one body; it also means a write that fails has to put the message back,
// which is what release does. A push claim takes only mail nobody has been
// handed yet: what the member already holds is its work in progress, and a
// message comes round again only when its lease runs out and the store puts
// it back on the queue.
func (c *courier) deliverTo(ctx context.Context, member *store.LoopMember) int {
	if member == nil || member.RunID == "" || member.Status == store.LoopMemberDismissed {
		return 0
	}
	if !c.claimMember(member.ID) {
		return 0
	}
	n, failed, stuck := c.deliver(ctx, member)
	if !stuck {
		c.releaseMember(member.ID, failed)
	}
	return n
}

// deliver does the work of deliverTo. stuck reports a write that timed out
// and is still in flight; the member stays busy until it returns.
func (c *courier) deliver(ctx context.Context, member *store.LoopMember) (delivered int, failed, stuck bool) {
	// Checked once per member, before anything is claimed: a loop over a
	// limit is about to be ended, and nothing claimed for it is left held.
	if c.budget != nil {
		if b, err := c.budget(ctx, member.LoopID); err == nil && b.Tripped {
			c.log.Info("courier: loop is over a limit, delivering nothing",
				"loop", member.LoopID, "member", member.Role, "reason", b.Reason)
			return 0, false, false
		}
	}
	msgs, err := c.st.ClaimInbox(ctx, member, store.ClaimOptions{Limit: courierBatch, Push: true})
	if err != nil {
		c.log.Warn("courier: claim", "member", member.ID, "err", err)
		return 0, true, false
	}
	for i, m := range msgs {
		done, err := c.write(ctx, member, renderMail(m))
		if done == nil && err == nil {
			if merr := c.st.MarkDelivered(ctx, member.ID, []string{m.ID}); merr != nil {
				c.log.Warn("courier: mark delivered", "member", member.ID, "msg", m.ID, "err", merr)
			}
			delivered++
			c.log.Info("courier: delivered mail into a terminal",
				"loop", m.LoopID, "to", member.Role, "from", m.SenderRole, "msg", m.ID)
			continue
		}
		// Everything after this message is unattempted and goes back on the
		// queue IN ORDER, so the member's own inbox endpoint and the stranded
		// detector still see it. Releasing is what stops a failed write from
		// silently swallowing mail.
		rest := make([]string, 0, len(msgs)-i)
		for _, r := range msgs[i+1:] {
			rest = append(rest, r.ID)
		}
		if done != nil {
			// The write is still stuck in the terminal. It may yet land, so
			// this message stays held (its lease brings it back if it never
			// does) and the member is released only when the write returns.
			c.log.Warn("courier: write into a terminal timed out", "member", member.ID,
				"run", member.RunID, "msg", m.ID, "after", c.writeTimeout.String())
			c.releaseIDs(ctx, member, rest)
			go c.settle(member, m.ID, done) //nolint:gosec // G118: settles after the delivery pass returns; must not inherit its cancellation
			return delivered, true, true
		}
		c.releaseIDs(ctx, member, append([]string{m.ID}, rest...))
		c.log.Debug("courier: deliver", "member", member.ID, "run", member.RunID,
			"msg", m.ID, "returned_to_queue", len(rest)+1, "err", err)
		return delivered, true, false
	}
	return delivered, false, false
}

func (c *courier) releaseIDs(ctx context.Context, member *store.LoopMember, ids []string) {
	if len(ids) == 0 {
		return
	}
	if _, err := c.st.ReleaseMail(ctx, ids); err != nil {
		c.log.Warn("courier: release", "member", member.ID, "err", err)
	}
}

// settle waits out a write that timed out, then counts it if it landed or
// returns the message to the queue if it did not, and frees the member.
func (c *courier) settle(member *store.LoopMember, msgID string, done <-chan error) {
	err := <-done
	ctx := context.Background()
	if err == nil {
		if merr := c.st.MarkDelivered(ctx, member.ID, []string{msgID}); merr != nil {
			c.log.Warn("courier: mark delivered", "member", member.ID, "msg", msgID, "err", merr)
		}
	} else {
		c.releaseIDs(ctx, member, []string{msgID})
	}
	c.releaseMember(member.ID, err != nil)
}

// write submits text with a deadline. It returns the write's error, or, when
// the deadline passed first, a channel that delivers the error once the
// write finally returns.
func (c *courier) write(ctx context.Context, member *store.LoopMember, text string) (<-chan error, error) {
	timeout := c.writeTimeout
	if timeout <= 0 {
		timeout = courierWriteTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	done := make(chan error, 1)
	go func() {
		defer cancel()
		done <- c.submitTo(wctx, member, text)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		return nil, err
	case <-t.C:
		return done, nil
	}
}

// submitTo writes one prompt into the member's process, by engine.
func (c *courier) submitTo(ctx context.Context, member *store.LoopMember, text string) error {
	switch store.NormaliseTool(member.Tool) {
	case store.ToolOpenCode:
		if c.sp == nil {
			return fmt.Errorf("courier: no spawner, so no opencode control")
		}
		run, err := c.sp.Status(ctx, member.RunID)
		if err != nil {
			return err
		}
		if run == nil || run.ControlBase == "" {
			return fmt.Errorf("courier: run %s has no control API", member.RunID)
		}
		ctl := opencode.NewControl(run.ControlBase)
		if run.SessionID != "" {
			ctl.BindSession(run.SessionID)
		}
		return ctl.Prompt(ctx, text)
	default:
		if c.submit == nil {
			return fmt.Errorf("courier: no tty writer")
		}
		return c.submit(member.RunID, text)
	}
}

// renderMail is what a member actually reads.
//
// It carries the message id because ack and extend name one, the sender
// because a reply needs an address, and the body verbatim. It says nothing
// about polling: the whole point is that the member does not.
//
// Subject and body pass through sanitizeForTTY, which strips C0 controls
// (except \n and \t), DEL, ESC, C1 controls and the bracketed-paste markers,
// and turns \r\n and \r into \n. Without it a sender could end the paste and
// type a command into another member's terminal: a body carrying
// "\x1b[201~\r/quit\r" must arrive as inert text. A body over 16 KB is
// replaced with a pointer so one message cannot flood a prompt.
func renderMail(m *store.LoopMessage) string {
	const maxBodyBytes = 16 * 1024
	subject := sanitizeForTTY(m.Subject, 256)
	body := m.Body
	if len(body) > maxBodyBytes {
		body = fmt.Sprintf("(message %s body is %d bytes; read it with the mail API)\n",
			m.ID, len(m.Body))
	} else {
		body = sanitizeForTTY(body, maxBodyBytes)
	}
	var b strings.Builder
	b.WriteString("[loop mail] from ")
	b.WriteString(m.SenderRole)
	if s := strings.TrimSpace(subject); s != "" {
		b.WriteString(" — ")
		b.WriteString(s)
	}
	b.WriteString(" (message ")
	b.WriteString(m.ID)
	b.WriteString(")\n")
	b.WriteString(body)
	return b.String()
}

// sanitizeForTTY strips terminal-control bytes from a string before it
// reaches the spawner's PTY. Whitelisted: \n, \t, ordinary printable
// runes. Everything else is dropped. \r\n and lone \r are normalised
// to \n so a body can never carry a literal CR that the TTY would
// turn into a Submit (\r is the TUI's "send" key).
func sanitizeForTTY(s string, max int) string {
	if s == "" {
		return s
	}
	if len(s) > max {
		s = s[:max]
	}
	// Normalise line endings first.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == 0x7f: // DEL
		case r == 0x1b: // ESC
		case r >= 0x80 && r <= 0x9f: // C1 controls
		default:
			// Drop other C0 controls (< 0x20) except already-handled.
			if r < 0x20 {
				continue
			}
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Strip bracketed-paste markers that survived any path.
	for _, marker := range []string{"\x1b[200~", "\x1b[201~"} {
		out = strings.ReplaceAll(out, marker, "")
	}
	return out
}

// Run is the delivery loop: wake on committed mail, sweep on a timer. Each
// member is delivered to on its own goroutine, at most courierWorkers at a
// time, so a terminal that stopped reading holds up only its own mail.
func (c *courier) Run(ctx context.Context) {
	mail, release := c.st.WatchMail()
	defer release()
	t := time.NewTicker(courierSweep)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	schedule := func(m *store.LoopMember) {
		select {
		case c.slots <- struct{}{}:
		default:
			return // every worker is busy; the next sweep comes back for it
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-c.slots }()
			c.deliverTo(ctx, m)
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case memberID := <-mail:
			m, err := c.st.LoopMemberByID(ctx, memberID)
			if err != nil || m == nil {
				continue
			}
			schedule(m)
		case now := <-t.C:
			c.forget(now)
			members, err := c.st.MembersAwaitingDelivery(ctx, 0)
			if err != nil {
				c.log.Warn("courier: sweep", "err", err)
				continue
			}
			for _, m := range members {
				schedule(m)
			}
		}
	}
}

// sweep delivers, one after another, to every member with mail waiting and a
// live run on this host. Run does the same through the worker pool.
func (c *courier) sweep(ctx context.Context) int {
	members, err := c.st.MembersAwaitingDelivery(ctx, 0)
	if err != nil {
		c.log.Warn("courier: sweep", "err", err)
		return 0
	}
	n := 0
	for _, m := range members {
		n += c.deliverTo(ctx, m)
	}
	return n
}
