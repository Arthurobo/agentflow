package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
)

// Access requests come from a browser that opened the pair page without a
// pairing link and tapped Request access. They live in the running daemon,
// so every command here goes through the admin socket.

// accessPoll is how often a terminal watching for access requests asks the
// daemon for new ones.
const accessPoll = 2 * time.Second

func runDecide(cfg config, target string, approve bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := decideAccessRequest(ctx, os.Stdout, adminClient(cfg), target, approve); err != nil {
		verb := "deny"
		if approve {
			verb = "approve"
		}
		fmt.Fprintf(os.Stderr, "agentflow %s: %v\n", verb, err)
		return 1
	}
	return 0
}

// decideAccessRequest approves or denies the pending request named by its id
// or its four-digit code.
func decideAccessRequest(ctx context.Context, w io.Writer, c *adminsock.Client, target string, approve bool) error {
	action := "deny"
	if approve {
		action = "approve"
	}
	var out adminsock.DecisionResponse
	err := c.Do(ctx, "POST", "/pair/requests/"+url.PathEscape(target)+"/"+action, &out)
	var apiErr *adminsock.APIError
	switch {
	case errors.Is(err, adminsock.ErrNotRunning):
		return errors.New("agentflow is not running, so there are no access requests (they only exist while it runs)")
	case errors.As(err, &apiErr) && apiErr.Status == 404:
		return fmt.Errorf("no pending access request %q; it may have expired (see `agentflow devices`)", target)
	case err != nil:
		return err
	}
	if approve {
		fmt.Fprintf(w, "Approved %s (code %s). It finishes pairing by itself in a few seconds.\n", out.Request.Name, out.Request.MatchCode)
	} else {
		fmt.Fprintf(w, "Denied %s (code %s).\n", out.Request.Name, out.Request.MatchCode)
	}
	return nil
}

// listAccessRequests prints the requests waiting for a decision, if the
// daemon is running and has any.
func listAccessRequests(ctx context.Context, w io.Writer, c *adminsock.Client, now time.Time) error {
	var out adminsock.PairRequestsResponse
	err := c.Do(ctx, "GET", "/pair/requests", &out)
	if errors.Is(err, adminsock.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(out.Requests) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Waiting for approval (agentflow approve CODE, or agentflow deny CODE):")
	fmt.Fprintf(w, "%-6s  %-24s  %-18s  %-12s  %s\n", "CODE", "NAME", "FROM", "ASKED", "ID")
	for _, r := range out.Requests {
		fmt.Fprintf(w, "%-6s  %-24s  %-18s  %-12s  %s\n",
			r.MatchCode, truncate(r.Name, 24), truncate(r.ClientIP, 18), sinceMillis(r.CreatedAt, now), r.ID)
	}
	return nil
}

// watchAccessRequests asks about every new access request as it arrives
// until ctx is done or in runs out. The default answer is no. It returns
// ctx.Err() when stopped by ctx and nil when the input ended.
func watchAccessRequests(ctx context.Context, in io.Reader, out io.Writer, c *adminsock.Client, poll time.Duration) error {
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	asked := map[string]bool{}
	for {
		var list adminsock.PairRequestsResponse
		if err := c.Do(ctx, "GET", "/pair/requests", &list); err == nil {
			for _, req := range list.Requests {
				if asked[req.ID] {
					continue
				}
				asked[req.ID] = true
				fmt.Fprintf(out, "\n%s (code %s) wants access — approve? [y/N] ", req.Name, req.MatchCode)
				var answer string
				select {
				case <-ctx.Done():
					fmt.Fprintln(out)
					return ctx.Err()
				case line, ok := <-lines:
					if !ok {
						fmt.Fprintln(out)
						return nil
					}
					answer = strings.ToLower(strings.TrimSpace(line))
				}
				approve := answer == "y" || answer == "yes"
				if err := decideAccessRequest(ctx, out, c, req.ID, approve); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					fmt.Fprintln(out, err)
				}
			}
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
	}
}

// stdinIsTerminal reports whether someone can answer prompts on stdin.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
