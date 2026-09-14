package cloud

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
	"strings"
	"time"
)

// Client talks to the account service.
type Client interface {
	RequestCode(ctx context.Context, email string) error
	Verify(ctx context.Context, email, code string) (token string, err error)
	Logout(ctx context.Context, token string) error
	PutMachine(ctx context.Context, token string, m Machine) error
	Heartbeat(ctx context.Context, token, machineID string) error
}

// Errors the account service reports, matched with errors.Is against an
// *APIError.
var (
	ErrInvalidEmail = errors.New("cloud: invalid email address")
	ErrInvalidCode  = errors.New("cloud: wrong or expired code")
	ErrRateLimited  = errors.New("cloud: too many requests")
	ErrUnauthorized = errors.New("cloud: not signed in")
	ErrNotFound     = errors.New("cloud: not found")
)

// APIError is an error response from the account service.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("account service: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("account service: HTTP %d", e.Status)
}

// Is maps the service's error codes to the package's sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrInvalidEmail:
		return e.Code == "invalid_email"
	case ErrInvalidCode:
		return e.Code == "invalid_code"
	case ErrRateLimited:
		return e.Status == http.StatusTooManyRequests
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	}
	return false
}

// requestTimeout bounds each call; the daemon retries on its own schedule.
const requestTimeout = 10 * time.Second

type httpClient struct {
	base string
	// baseErr is set when base isn't safe to send a token to.
	baseErr error
	http    *http.Client
}

// NewClient returns a client for baseURL (AF_CLOUD_URL, or DefaultBaseURL).
// An empty baseURL means DefaultBaseURL. Plain http is refused except to a
// loopback address, so a mistyped AF_CLOUD_URL can't send a token in clear
// text.
func NewClient(baseURL string) Client {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &httpClient{base: base, baseErr: checkBaseURL(base), http: &http.Client{Timeout: requestTimeout}}
}

func checkBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("cloud: account service URL %q is not an absolute URL", base)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("cloud: account service URL %q must use https", base)
}

func (c *httpClient) RequestCode(ctx context.Context, email string) error {
	return c.do(ctx, http.MethodPost, "/v1/auth/code", "", map[string]string{"email": email}, nil)
}

func (c *httpClient) Verify(ctx context.Context, email, code string) (string, error) {
	var resp struct {
		Token string `json:"token"`
	}
	body := map[string]string{"email": email, "code": code, "client": "cli"}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/verify", "", body, &resp); err != nil {
		return "", err
	}
	if resp.Token == "" {
		return "", errors.New("cloud: account service returned no token")
	}
	return resp.Token, nil
}

// Logout revokes token. A token the service no longer knows counts as logged
// out.
func (c *httpClient) Logout(ctx context.Context, token string) error {
	err := c.do(ctx, http.MethodPost, "/v1/auth/logout", token, nil, nil)
	if errors.Is(err, ErrUnauthorized) {
		return nil
	}
	return err
}

func (c *httpClient) PutMachine(ctx context.Context, token string, m Machine) error {
	body := struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		URL       string `json:"url"`
		Version   string `json:"agentflowVersion"`
	}{m.Name, m.Transport, m.URL, m.Version}
	return c.do(ctx, http.MethodPut, "/v1/machines/"+url.PathEscape(m.ID), token, body, nil)
}

func (c *httpClient) Heartbeat(ctx context.Context, token, machineID string) error {
	return c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/heartbeat", token, nil, nil)
}

// maxResponse bounds what is read from the service.
const maxResponse = 64 << 10

func (c *httpClient) do(ctx context.Context, method, path, token string, body, out any) error {
	if c.baseErr != nil {
		return c.baseErr
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "agentflow/"+Version)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloud: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("cloud: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil {
			apiErr.Code, apiErr.Message = env.Error.Code, env.Error.Message
		}
		return apiErr
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("cloud: %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}
