package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/arthurobo/agentflow/internal/cloud"
	"github.com/arthurobo/agentflow/internal/remote"
)

// accountReporter keeps the account service told about this machine's public
// URL while agentflow runs: it runs cloud.RunReporter for the stored account,
// fed by the transport's status. Reload picks up a sign-in or sign-out that
// happened after the daemon started, so neither needs a restart.
type accountReporter struct {
	dataDir   string
	transport string // "" when remote access is off
	name      string
	version   string
	rem       remote.Transport
	client    func() cloud.Client
	log       *slog.Logger
	// report is cloud.RunReporter; tests replace it.
	report func(ctx context.Context, c cloud.Client, acct *cloud.Account, m cloud.Machine, status <-chan cloud.URLStatus, log *slog.Logger)

	mu     sync.Mutex
	parent context.Context
	cancel context.CancelFunc
	email  string
}

// accountState is what the admin socket reports about reporting.
type accountState struct {
	Reporting bool   `json:"reporting"`
	Email     string `json:"email,omitempty"`
}

// Start begins reporting for the stored account, if any, until ctx is done.
func (a *accountReporter) Start(ctx context.Context) {
	a.mu.Lock()
	a.parent = ctx
	a.mu.Unlock()
	a.Reload()
}

// Reload stops any reporting in progress and starts again from the account
// file as it is now.
func (a *accountReporter) Reload() accountState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.email = ""
	if a.parent == nil || a.parent.Err() != nil {
		return a.stateLocked()
	}
	if a.transport == "" {
		return a.stateLocked()
	}
	acct, err := cloud.LoadAccount(a.dataDir)
	if err != nil {
		a.log.Warn("agentflow: can't read the account file; this machine's link won't be reported", "err", err)
		return a.stateLocked()
	}
	if acct == nil {
		return a.stateLocked()
	}
	id, err := cloud.MachineID(a.dataDir)
	if err != nil {
		a.log.Warn("agentflow: no machine id; this machine's link won't be reported", "err", err)
		return a.stateLocked()
	}

	ctx, cancel := context.WithCancel(a.parent)
	a.cancel = cancel
	a.email = acct.Email

	// Subscribe before reading the snapshot, so no change falls between.
	sub, unsubscribe := a.rem.Subscribe()
	m := cloud.Machine{ID: id, Name: a.name, Transport: a.transport, Version: a.version}
	if st := a.rem.Status(); st.State == remote.StateRunning {
		m.URL = st.PublicURL
	}
	urls := make(chan cloud.URLStatus, 1)
	go func() {
		defer unsubscribe()
		last := m.URL
		for {
			select {
			case <-ctx.Done():
				return
			case st, ok := <-sub:
				if !ok {
					return
				}
				// Only a working URL is news. Reporting the gaps while a
				// transport reconnects would read as a new link each time
				// it comes back, and email the person again.
				if st.State != remote.StateRunning || st.PublicURL == "" || st.PublicURL == last {
					continue
				}
				last = st.PublicURL
				select {
				case urls <- cloud.URLStatus{Transport: a.transport, URL: st.PublicURL}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go a.report(ctx, a.client(), acct, m, urls, a.log)
	return a.stateLocked()
}

// State reports whether the account service is being told about this
// machine, and as whom.
func (a *accountReporter) State() accountState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

func (a *accountReporter) stateLocked() accountState {
	return accountState{Reporting: a.cancel != nil, Email: a.email}
}
