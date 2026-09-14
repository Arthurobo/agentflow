package agentapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arthurobo/agentflow/internal/httpserve/ratelimit"
)

// Pairing by approval. A phone that has no pairing link asks for access; the
// person at the computer sees the request (with a four-digit code that is
// also on the phone, so they approve the right one) and approves or denies
// it; the phone, polling with a secret only it holds, picks up its device
// token once.
//
// Requests live in memory: they last minutes, and a daemon restart simply
// means asking again.

// PairRequestTTL is how long a request waits for a decision.
const PairRequestTTL = 5 * time.Minute

const (
	// maxPendingPairRequests bounds undecided requests. Anyone who can load
	// the pair page can ask, so the list a person has to read stays short.
	maxPendingPairRequests = 5
	// maxPendingPerClient keeps one visitor from filling every slot and
	// shutting out the person who is actually trying to pair.
	maxPendingPerClient = 2
	maxPairRequestName  = 64
	// pairRequestLinger keeps a decided or expired request answerable for a
	// while, so the phone polling it learns the outcome instead of a 404.
	pairRequestLinger = PairRequestTTL
)

// PairRequestPollHeader carries the poll secret. A header rather than the
// query string keeps it out of access logs.
const PairRequestPollHeader = "X-Pair-Secret"

// Request states as the poller sees them.
const (
	PairRequestPending  = "pending"
	PairRequestApproved = "approved"
	PairRequestDenied   = "denied"
	PairRequestExpired  = "expired"
)

var (
	// ErrPairRequestNotFound means no pending request has that id or code.
	ErrPairRequestNotFound = errors.New("no pending access request")
	errTooManyPairRequests = errors.New("too many pending access requests")
)

// PairRequestInfo is what the person approving a request is shown.
type PairRequestInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MatchCode string `json:"matchCode"`
	ClientIP  string `json:"clientIp"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

type pairRequest struct {
	info       PairRequestInfo
	secretHash [sha256.Size]byte
	status     string // pending, approved or denied
	decidedAt  time.Time
}

type pairRequests struct {
	mu   sync.Mutex
	now  func() time.Time
	byID map[string]*pairRequest
}

func newPairRequests() *pairRequests {
	return &pairRequests{now: time.Now, byID: map[string]*pairRequest{}}
}

// expired reports whether an undecided request ran out of time.
func (p *pairRequests) expired(req *pairRequest, now time.Time) bool {
	return req.status == PairRequestPending && now.UnixMilli() >= req.info.ExpiresAt
}

func (p *pairRequests) sweepLocked(now time.Time) {
	for id, req := range p.byID {
		end := time.UnixMilli(req.info.ExpiresAt)
		if !req.decidedAt.IsZero() {
			end = req.decidedAt
		}
		if now.After(end.Add(pairRequestLinger)) {
			delete(p.byID, id)
		}
	}
}

// create records a new pending request and returns it with its poll secret.
func (p *pairRequests) create(name, clientIP string) (PairRequestInfo, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLocked(now)
	pending, fromClient := 0, 0
	codes := map[string]bool{}
	for _, req := range p.byID {
		codes[req.info.MatchCode] = true
		if req.status == PairRequestPending && !p.expired(req, now) {
			pending++
			if req.info.ClientIP == clientIP {
				fromClient++
			}
		}
	}
	if pending >= maxPendingPairRequests || fromClient >= maxPendingPerClient {
		return PairRequestInfo{}, "", errTooManyPairRequests
	}
	code, err := uniqueMatchCode(codes)
	if err != nil {
		return PairRequestInfo{}, "", err
	}
	secret := newSecret(24) // 192 bits
	req := &pairRequest{
		info: PairRequestInfo{
			ID:        newSecret(16), // 128 bits
			Name:      name,
			MatchCode: code,
			ClientIP:  clientIP,
			CreatedAt: now.UnixMilli(),
			ExpiresAt: now.Add(PairRequestTTL).UnixMilli(),
		},
		secretHash: sha256.Sum256([]byte(secret)),
		status:     PairRequestPending,
	}
	p.byID[req.info.ID] = req
	return req.info, secret, nil
}

// uniqueMatchCode picks a four-digit code no live request is using, so a
// code names one request.
func uniqueMatchCode(taken map[string]bool) (string, error) {
	for range 50 {
		n, err := rand.Int(rand.Reader, big.NewInt(10000))
		if err != nil {
			return "", err
		}
		code := fmt.Sprintf("%04d", n.Int64())
		if !taken[code] {
			return code, nil
		}
	}
	return "", errors.New("no free match code")
}

// poll returns the state of the request for its holder. It reports found
// false for an unknown id or a wrong secret alike, so a poll can't be used
// to learn which requests exist. An approved request is removed as it is
// returned: the caller must deliver the device token or call restore.
func (p *pairRequests) poll(id, secret string) (status string, info PairRequestInfo, found bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLocked(now)
	req := p.byID[id]
	if req == nil {
		return "", PairRequestInfo{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	if secret == "" || subtle.ConstantTimeCompare(sum[:], req.secretHash[:]) != 1 {
		return "", PairRequestInfo{}, false
	}
	switch {
	case p.expired(req, now):
		return PairRequestExpired, req.info, true
	case req.status == PairRequestApproved:
		delete(p.byID, id)
		return PairRequestApproved, req.info, true
	default:
		return req.status, req.info, true
	}
}

// restore puts back an approved request whose device token could not be
// issued, so the next poll tries again.
func (p *pairRequests) restore(info PairRequestInfo, secret string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byID[info.ID] = &pairRequest{
		info: info, secretHash: sha256.Sum256([]byte(secret)),
		status: PairRequestApproved, decidedAt: p.now(),
	}
}

// decide approves or denies the pending request named by its id or its
// match code.
func (p *pairRequests) decide(idOrCode string, approve bool) (PairRequestInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLocked(now)
	for _, req := range p.byID {
		if req.status != PairRequestPending || p.expired(req, now) {
			continue
		}
		if req.info.ID != idOrCode && req.info.MatchCode != idOrCode {
			continue
		}
		req.status = PairRequestDenied
		if approve {
			req.status = PairRequestApproved
		}
		req.decidedAt = now
		return req.info, nil
	}
	return PairRequestInfo{}, ErrPairRequestNotFound
}

// list returns the undecided requests, oldest first.
func (p *pairRequests) list() []PairRequestInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.sweepLocked(now)
	out := []PairRequestInfo{}
	for _, req := range p.byID {
		if req.status == PairRequestPending && !p.expired(req, now) {
			out = append(out, req.info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListPairRequests returns the access requests waiting for a decision.
func (s *Server) ListPairRequests() []PairRequestInfo { return s.pairReqs.list() }

// DecidePairRequest approves or denies a pending access request by its id or
// its four-digit code. by says who decided, for the audit log. It returns
// ErrPairRequestNotFound when nothing pending matches.
func (s *Server) DecidePairRequest(idOrCode string, approve bool, by string) (PairRequestInfo, error) {
	info, err := s.pairReqs.decide(strings.TrimSpace(idOrCode), approve)
	if err != nil {
		return info, err
	}
	msg := "pair: access request denied"
	if approve {
		msg = "pair: access request approved"
	}
	s.log.Info(msg, "requestId", info.ID, "name", info.Name, "matchCode", info.MatchCode,
		"clientIp", info.ClientIP, "by", by)
	return info, nil
}

// cleanDeviceName makes a requester-supplied label safe to show in a
// terminal and a list: no control or format characters, bounded length.
func cleanDeviceName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > maxPairRequestName {
		name = string([]rune(name)[:maxPairRequestName])
	}
	if name == "" {
		return "Unnamed device"
	}
	return name
}

// handlePairRequestCreate is POST /pair/request: an unpaired browser asks
// for access. Attempts share the pairing rate limit.
func (s *Server) handlePairRequestCreate(w http.ResponseWriter, r *http.Request) {
	ip := ratelimit.ClientIP(r)
	if !s.pairLimiter.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "pair rate exceeded; try again in a minute")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}
	name := cleanDeviceName(body.Name)
	info, secret, err := s.pairReqs.create(name, ip)
	if errors.Is(err, errTooManyPairRequests) {
		writeError(w, http.StatusTooManyRequests, "too_many_requests",
			"too many access requests are waiting; approve or deny them on the computer, or try again in a few minutes")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.log.Info("pair: access requested", "requestId", info.ID, "name", name,
		"matchCode", info.MatchCode, "clientIp", ip, "userAgent", r.UserAgent())
	writeJSON(w, http.StatusCreated, map[string]any{
		"requestId":  info.ID,
		"pollSecret": secret, // returned once; only its hash is kept
		"matchCode":  info.MatchCode,
		"expiresAt":  info.ExpiresAt,
	})
}

// handlePairRequestPoll is GET /pair/request/{id}: the requester learns the
// decision, and on approval receives its device token exactly once.
func (s *Server) handlePairRequestPoll(w http.ResponseWriter, r *http.Request) {
	secret := r.Header.Get(PairRequestPollHeader)
	status, info, found := s.pairReqs.poll(r.PathValue("id"), secret)
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such access request")
		return
	}
	if status != PairRequestApproved {
		writeJSON(w, http.StatusOK, map[string]any{"status": status})
		return
	}
	devID, token, err := s.registerDevice(r.Context(), info.Name)
	if err != nil {
		s.pairReqs.restore(info, secret)
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	setDeviceCookie(w, r, token)
	s.log.Info("pair: device registered",
		"deviceId", devID, "name", info.Name, "requestId", info.ID,
		"clientIp", ratelimit.ClientIP(r), "machineId", s.machineID, "userAgent", r.UserAgent())
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      PairRequestApproved,
		"deviceId":    devID,
		"deviceToken": token, // returned exactly once; only its hash is stored
		"machineId":   s.machineID,
	})
}

// handlePairRequestsList is GET /pair/requests for a paired browser on the
// local listener.
func (s *Server) handlePairRequestsList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"requests": s.ListPairRequests()})
}

// pairRequestDecider answers POST /pair/requests/{id}/approve or /deny for
// a paired browser on the local listener.
func (s *Server) pairRequestDecider(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		by := "local browser"
		if d := deviceFrom(r.Context()); d != nil {
			by = "device " + d.ID
		}
		info, err := s.DecidePairRequest(r.PathValue("id"), approve, by)
		if errors.Is(err, ErrPairRequestNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no pending access request with that id")
			return
		}
		decision := PairRequestDenied
		if approve {
			decision = PairRequestApproved
		}
		writeJSON(w, http.StatusOK, map[string]any{"request": info, "decision": decision})
	}
}
