package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrTooManyAttempts is returned by PromptEmail after three wrong codes.
var ErrTooManyAttempts = errors.New("cloud: too many wrong codes")

// codeAttempts is how many wrong codes PromptEmail accepts before giving up.
const codeAttempts = 3

// PromptEmail asks for an email on out, sends a code, reads the code from in,
// verifies it and saves the account. With required=false an empty answer, or
// input that ends before an account is saved, returns ErrSkipped; with
// required=true the end of input is an error.
func PromptEmail(ctx context.Context, in io.Reader, out io.Writer, c Client, dataDir string, required bool) (*Account, error) {
	lines := newLineReader(in)
	ended := func() error {
		if required {
			return errors.New("input ended before sign-in finished")
		}
		return ErrSkipped
	}

	prompt := "Email for your agentflow link and dashboard (optional, press Enter to skip): "
	if required {
		prompt = "Email for your agentflow link and dashboard: "
	}

	var email string
	for email == "" {
		fmt.Fprint(out, prompt)
		line, err := lines.read(ctx)
		if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(out)
				return nil, ended()
			}
			return nil, err
		}
		// Normalize once so the code request and the verification always
		// name the same address.
		line = strings.ToLower(strings.TrimSpace(line))
		switch {
		case line == "" && !required:
			return nil, ErrSkipped
		case line == "":
			fmt.Fprintln(out, "Type the email to sign in with, or press Ctrl-C to cancel.")
			continue
		case !looksLikeEmail(line):
			fmt.Fprintln(out, "That doesn't look like an email address.")
			continue
		}
		if err := c.RequestCode(ctx, line); err != nil {
			switch {
			case errors.Is(err, ErrInvalidEmail):
				fmt.Fprintln(out, "The account service didn't accept that address.")
				continue
			case errors.Is(err, ErrRateLimited):
				return nil, errors.New("too many sign-in codes were requested for this email; wait a few minutes and try again")
			default:
				return nil, fmt.Errorf("send a sign-in code: %w", err)
			}
		}
		email = line
	}

	fmt.Fprintf(out, "Code sent to %s. Enter the 6-digit code: ", email)
	wrong := 0
	for {
		line, err := lines.read(ctx)
		if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(out)
				return nil, ended()
			}
			return nil, err
		}
		code := strings.TrimSpace(line)
		switch {
		case code == "":
			fmt.Fprint(out, `Enter the 6-digit code (or "resend" for a new one): `)
			continue
		case strings.EqualFold(code, "resend"):
			if err := c.RequestCode(ctx, email); err != nil {
				if errors.Is(err, ErrRateLimited) {
					fmt.Fprint(out, "Too many codes were requested; use the last one you received, or wait a few minutes. Enter the 6-digit code: ")
					continue
				}
				return nil, fmt.Errorf("send a sign-in code: %w", err)
			}
			fmt.Fprintf(out, "New code sent to %s. Enter the 6-digit code: ", email)
			continue
		}

		token, err := c.Verify(ctx, email, strings.ReplaceAll(code, " ", ""))
		if errors.Is(err, ErrInvalidCode) {
			wrong++
			if wrong >= codeAttempts {
				fmt.Fprintln(out, "That code didn't work either.")
				return nil, fmt.Errorf("%w; run `agentflow account login` to try again", ErrTooManyAttempts)
			}
			fmt.Fprint(out, `That code didn't work. Enter the 6-digit code (or "resend" for a new one): `)
			continue
		}
		if errors.Is(err, ErrRateLimited) {
			return nil, errors.New("too many sign-in attempts from this network; wait a while and run `agentflow account login`")
		}
		if err != nil {
			return nil, fmt.Errorf("verify the code: %w", err)
		}

		id, err := MachineID(dataDir)
		if err != nil {
			return nil, err
		}
		acct := &Account{Email: email, Token: token, MachineID: id, VerifiedAt: time.Now().UTC()}
		if err := SaveAccount(dataDir, acct); err != nil {
			return nil, fmt.Errorf("save the account: %w", err)
		}
		fmt.Fprintf(out, "Signed in as %s.\n", email)
		return acct, nil
	}
}

// looksLikeEmail is a quick local check so a typo doesn't cost a round trip;
// the service does the real validation.
func looksLikeEmail(s string) bool {
	if len(s) > 254 || strings.ContainsAny(s, " \t\"<>(),;:\\") {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") && !strings.Contains(s[:at], "@")
}

// lineReader reads one line at a time without reading ahead, so whatever the
// caller reads from the same input afterwards (the next prompt of
// `agentflow start`) is still there.
type lineReader struct {
	in      io.Reader
	pending chan lineResult // a read still running after its context ended
}

type lineResult struct {
	line string
	err  error
}

func newLineReader(in io.Reader) *lineReader { return &lineReader{in: in} }

// read returns the next line without its newline. A read blocked on a
// terminal can't be interrupted, so when ctx ends first read returns
// ctx.Err() and the next call picks up that line.
func (r *lineReader) read(ctx context.Context) (string, error) {
	ch := r.pending
	if ch == nil {
		ch = make(chan lineResult, 1)
		go func() {
			var b strings.Builder
			var buf [1]byte
			for {
				n, err := r.in.Read(buf[:])
				if n == 1 {
					if buf[0] == '\n' {
						ch <- lineResult{strings.TrimSuffix(b.String(), "\r"), nil}
						return
					}
					b.WriteByte(buf[0])
					if b.Len() > 1024 {
						ch <- lineResult{"", errors.New("input line too long")}
						return
					}
				}
				if err != nil {
					ch <- lineResult{b.String(), err}
					return
				}
			}
		}()
	}
	select {
	case res := <-ch:
		r.pending = nil
		return res.line, res.err
	case <-ctx.Done():
		r.pending = ch
		return "", ctx.Err()
	}
}
