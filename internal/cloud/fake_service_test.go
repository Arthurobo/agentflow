package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeService is an httptest stand-in for the account service API: it keeps
// one account's codes, tokens and machines in memory.
type fakeService struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	codes      map[string]string // email -> current code
	codeSends  map[string]int
	tokens     map[string]string // token -> email
	machines   map[string]map[string]string
	puts       []map[string]string
	heartbeats int
	logouts    int
	requests   []*http.Request
	rawEmails  []string // "code:<email>" / "verify:<email>" exactly as sent
	// failNext answers the next n API calls with 503.
	failNext int
	// rateLimitCodes answers code requests with 429.
	rateLimitCodes bool
}

func newFakeService(t *testing.T) *fakeService {
	f := &fakeService{
		t:         t,
		codes:     map[string]string{},
		codeSends: map[string]int{},
		tokens:    map[string]string{},
		machines:  map[string]map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/code", f.code)
	mux.HandleFunc("POST /v1/auth/verify", f.verify)
	mux.HandleFunc("POST /v1/auth/logout", f.authed(f.logout))
	mux.HandleFunc("PUT /v1/machines/{id}", f.authed(f.put))
	mux.HandleFunc("POST /v1/machines/{id}/heartbeat", f.authed(f.heartbeat))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(r.Context()))
		fail := f.failNext > 0
		if fail {
			f.failNext--
		}
		f.mu.Unlock()
		if fail {
			apiError(w, 503, "unavailable", "try later")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func decodeBody(r *http.Request) map[string]string {
	m := map[string]string{}
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

func (f *fakeService) code(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	email := strings.ToLower(strings.TrimSpace(body["email"]))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawEmails = append(f.rawEmails, "code:"+body["email"])
	if f.rateLimitCodes {
		apiError(w, 429, "rate_limited", "too many requests")
		return
	}
	if !strings.Contains(email, "@") || strings.HasSuffix(email, "@invalid.example") {
		apiError(w, 400, "invalid_email", "bad email")
		return
	}
	f.codeSends[email]++
	f.codes[email] = []string{"111111", "222222", "333333"}[(f.codeSends[email]-1)%3]
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("{}"))
}

func (f *fakeService) verify(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawEmails = append(f.rawEmails, "verify:"+body["email"])
	if body["client"] != "cli" {
		apiError(w, 400, "invalid_request", "client")
		return
	}
	if want, ok := f.codes[body["email"]]; !ok || want != body["code"] {
		apiError(w, 400, "invalid_code", "the code is wrong or has expired")
		return
	}
	token := strings.Repeat("t", 43)
	f.tokens[token] = body["email"]
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "user": map[string]string{"id": "u1", "email": body["email"]}})
}

func (f *fakeService) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		_, ok := f.tokens[token]
		f.mu.Unlock()
		if !ok {
			apiError(w, 401, "unauthorized", "sign in first")
			return
		}
		next(w, r)
	}
}

func (f *fakeService) logout(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	f.logouts++
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeService) put(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	id := r.PathValue("id")
	if id == "taken" {
		apiError(w, 404, "not_found", "not found")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body["id"] = id
	f.machines[id] = body
	f.puts = append(f.puts, body)
	_ = json.NewEncoder(w).Encode(map[string]any{"machine": body})
}

func (f *fakeService) heartbeat(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.machines[r.PathValue("id")]; !ok {
		apiError(w, 404, "not_found", "not found")
		return
	}
	f.heartbeats++
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeService) setRateLimit(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimitCodes = on
}
