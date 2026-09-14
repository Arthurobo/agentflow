package adminsock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// ErrNotRunning means no daemon is listening on the socket.
var ErrNotRunning = errors.New("agentflow is not running")

// Client talks to a daemon's admin socket.
type Client struct {
	path string
	hc   *http.Client
}

// NewClient returns a client for the socket at path. It does not connect.
func NewClient(path string) *Client {
	return &Client{
		path: path,
		hc: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

// Path returns the socket path.
func (c *Client) Path() string { return c.path }

// APIError is a non-2xx answer from the daemon.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("admin socket: HTTP %d", e.Status)
}

// Do sends a request and decodes a JSON answer into out (which may be nil).
// A missing socket or a refused connection is ErrNotRunning.
func (c *Client) Do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://agentflow"+(&url.URL{Path: path}).EscapedPath(), bytes.NewReader(nil))
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if isNotRunning(err) {
			return ErrNotRunning
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: resp.StatusCode}
		var eb errorBody
		if json.Unmarshal(body, &eb) == nil {
			apiErr.Code, apiErr.Message = eb.Error.Code, eb.Error.Message
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func isNotRunning(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return errors.Is(err, os.ErrNotExist)
}
