package store

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
)

// EventRow is one row of the events table — the read shape served by the API.
type EventRow struct {
	ID                int64   `json:"id"`
	SessionID         string  `json:"sessionId"`
	OriginSessionID   string  `json:"originSessionId,omitempty"`
	Event             string  `json:"event"`
	Subtype           string  `json:"subtype,omitempty"`
	Actor             string  `json:"actor,omitempty"`
	UUID              string  `json:"uuid,omitempty"`
	ParentUUID        string  `json:"parentUuid,omitempty"`
	Seq               int64   `json:"seq"`
	TS                int64   `json:"ts"`
	ToolName          string  `json:"toolName,omitempty"`
	ToolUseID         string  `json:"toolUseId,omitempty"`
	ToolInput         string  `json:"toolInputJson,omitempty"`
	Content           string  `json:"content"`
	ThinkingContent   string  `json:"thinkingContent,omitempty"`
	ContentHash       string  `json:"contentHash,omitempty"`
	Oversized         bool    `json:"oversized,omitempty"`
	IsSidechain       bool    `json:"isSidechain,omitempty"`
	IsMeta            bool    `json:"isMeta,omitempty"`
	Retracted         bool    `json:"retracted,omitempty"`
	HasThinking       bool    `json:"hasThinking,omitempty"`
	ThinkingSignature string  `json:"thinkingSignature,omitempty"`
	Model             string  `json:"model,omitempty"`
	TokensIn          int64   `json:"tokensIn,omitempty"`
	TokensOut         int64   `json:"tokensOut,omitempty"`
	TokensCacheRead   int64   `json:"tokensCacheRead,omitempty"`
	TokensCacheWrite  int64   `json:"tokensCacheWrite,omitempty"`
	CostUSD           float64 `json:"costUsd,omitempty"`
	AgentID           string  `json:"agentId,omitempty"`
	RetractedUuids    any     `json:"retractedUuids,omitempty"`
	IsError           bool    `json:"isError,omitempty"`
	TerminalReason    string  `json:"terminalReason,omitempty"`
	DurationMs        int64   `json:"durationMs,omitempty"`
	RawVersion        string  `json:"rawVersion,omitempty"`
	Source            string  `json:"source,omitempty"`

	// LatencyMs is computed at query time: ts delta to the previous row in
	// the returned page (0 for the first row of a page).
	LatencyMs int64 `json:"latencyMs,omitempty"`
}

// Incoming is one event handed from ingest to the store for insertion.
// It wraps a normalized claudelog event plus the ingest-time extras the SQL
// schema needs (storage key, blob content).
type Incoming struct {
	SessionID       string
	OriginSessionID string
	Project         string
	CWD             string
	FilePath        string
	IsSubagent      bool
	Event           string
	Subtype         string
	Actor           string
	UUID            string
	ParentUUID      string
	Seq             int64
	TS              int64
	ToolName        string
	ToolUseID       string
	ToolInput       []byte
	Content         string
	ThinkingContent string
	ContentHash     string
	Oversized       bool
	IsSidechain     bool
	IsMeta          bool
	Retracted       bool
	HasThinking     bool
	ThinkingSig     string
	Model           string
	TokensIn        int64
	TokensOut       int64
	TokCacheRead    int64
	TokCacheWr      int64
	CostUSD         float64
	AgentID         string
	FileOffset      int64
	RawVersion      string
	IsError         bool
	TermReason      string
	DurationMs      int64
	PermDenials     int
	Source          string
	// BlobContent is the full content for oversized events.
	BlobContent string
	// BlobSize is len(BlobContent) when present.
	BlobSize int64
}

// Key returns the storage dedupe key: the canonical
// (sessionId, uuid|toolUseId|contentHash, event) tuple for identity-bearing
// events, and that tuple plus a stable per-file|seq discriminator for
// identity-less events (snapshot + queue-op records carry no uuid/hash).
// The file discriminator is required because subagent transcripts share the
// parent sessionId: without it, snapshot records at identical line numbers in
// different files of one session collapse into one row (1,603 events lost,
// measured on the full corpus before this fix).
func (ev *Incoming) Key() string {
	id := ev.UUID
	if id == "" {
		id = ev.ToolUseID
	}
	if id == "" {
		id = ev.ContentHash
	}
	if id == "" {
		return ev.SessionID + ",-," + ev.Event + "|" + fileTag(ev.FilePath) +
			":" + strconv.FormatInt(ev.Seq, 10)
	}
	return ev.SessionID + "," + id + "," + ev.Event
}

// fileTag is a short stable hash of a transcript path used in identity-less
// keys (8 hex chars — collision probability negligible at ~400 files).
func fileTag(path string) string {
	if path == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(path))
	const digits = "0123456789abcdef"
	var b [8]byte
	for i := range b {
		b[i] = digits[sum[i]>>4]
	}
	return string(b[:])
}

// Session is one row of the sessions table.
type Session struct {
	ID                string     `json:"id"`
	Project           string     `json:"project,omitempty"`
	Cwd               string     `json:"cwd,omitempty"`
	IsSubagent        bool       `json:"isSubagent,omitempty"`
	FilePath          string     `json:"filePath,omitempty"`
	StartedAt         int64      `json:"startedAt,omitempty"`
	EndedAt           int64      `json:"endedAt,omitempty"`
	UpdatedAt         int64      `json:"updatedAt,omitempty"`
	Title             string     `json:"title,omitempty"`
	IsActive          bool       `json:"isActive,omitempty"`
	FirstPrompt       string     `json:"firstPrompt,omitempty"`
	EventCount        int64      `json:"eventCount"`
	UserMessageCount  int64      `json:"userMessageCount,omitempty"`
	AssistantCount    int64      `json:"assistantMessageCount,omitempty"`
	ToolUseCount      int64      `json:"toolUseCount,omitempty"`
	ToolResultCount   int64      `json:"toolResultCount,omitempty"`
	SessionStateCount int64      `json:"sessionStateCount,omitempty"`
	QueueOpCount      int64      `json:"queueOpCount,omitempty"`
	PermissionDenials int64      `json:"permissionDenials,omitempty"`
	TotalCostUSD      float64    `json:"totalCostUsd,omitempty"`
	TokensIn          int64      `json:"tokensIn,omitempty"`
	TokensOut         int64      `json:"tokensOut,omitempty"`
	TokCacheRead      int64      `json:"tokensCacheRead,omitempty"`
	TokCacheWrite     int64      `json:"tokensCacheWrite,omitempty"`
	ModelMix          []ModelMix `json:"modelMix,omitempty"`
	PrimaryModel      string     `json:"primaryModel,omitempty"`
	// Engine is derived, not stored: the corpus predates the second engine.
	// See sessionEngineExpr for the two signals it comes from. On a MANAGED
	// row it is the managed_sessions.engine column instead, which is
	// recorded at spawn and needs no derivation.
	Engine          string `json:"engine,omitempty"`
	TerminalReason  string `json:"terminalReason,omitempty"`
	CompactionCount int64  `json:"compactionCount,omitempty"`
	// Managed marks a row that came from managed_sessions rather than the
	// corpus: an Agent-Flow-spawned run whose transcript has not been
	// ingested (every OpenCode run, and any Claude run before the tailer
	// catches up). Its counts are zero because it has no events yet, and
	// its liveness comes from ManagedState rather than from recency.
	Managed bool `json:"managed,omitempty"`
	// ManagedState is the run's lifecycle state (starting | running |
	// awaiting | finished | stopped | crashed). Empty on a corpus row,
	// which has no lifecycle to report.
	ManagedState string `json:"managedState,omitempty"`
}

// ModelMix is one element of sessions.model_mix_json.
type ModelMix struct {
	Model      string  `json:"model"`
	Messages   int64   `json:"messages"`
	TokensIn   int64   `json:"tokensIn,omitempty"`
	TokensOut  int64   `json:"tokensOut,omitempty"`
	CacheRead  int64   `json:"cacheRead,omitempty"`
	CacheWrite int64   `json:"cacheWrite,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"`
}

// UnmarshalModelMix parses the stored model_mix_json.
func UnmarshalModelMix(raw string) []ModelMix {
	var m []ModelMix
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// Project is one row of the projects table.
type Project struct {
	Name         string  `json:"name"`
	Cwd          string  `json:"cwd,omitempty"`
	SessionCount int64   `json:"sessionCount"`
	EventCount   int64   `json:"eventCount"`
	TotalCostUSD float64 `json:"totalCostUsd,omitempty"`
	TokensIn     int64   `json:"tokensIn,omitempty"`
	TokensOut    int64   `json:"tokensOut,omitempty"`
	FirstSeen    int64   `json:"firstSeen,omitempty"`
	LastSeen     int64   `json:"lastSeen,omitempty"`
}

// FileChange is one file touched by Edit events in a session (diffs view).
type FileChange struct {
	FilePath   string `json:"filePath"`
	EditCount  int64  `json:"editCount"`
	Additions  int64  `json:"additions,omitempty"`
	Deletions  int64  `json:"deletions,omitempty"`
	LastEditTs int64  `json:"lastEditTs,omitempty"`
	// Diff is a bounded unified-style diff of the first edit pair, empty if
	// impractical (huge old/new strings).
	Diff string `json:"diff,omitempty"`
}

// GitStatus is the cached per-cwd git state (diffs view chips).
type GitStatus struct {
	Repo      string     `json:"repo,omitempty"`
	Branch    string     `json:"branch,omitempty"`
	Entries   []GitEntry `json:"entries"`
	CheckedAt int64      `json:"checkedAt,omitempty"`
}

// GitEntry is one `git status --porcelain=v2 --branch` entry (simplified XY).
type GitEntry struct {
	Path string `json:"path"`
	XY   string `json:"xy"`
}

// ToolFreq is one (tool, count) pair for stats.
type ToolFreq struct {
	Tool     string `json:"tool"`
	Uses     int64  `json:"uses"`
	Sessions int64  `json:"sessions"`
}

// BashCommandStat is one parsed bash command stat for stats.
type BashCommandStat struct {
	Command string `json:"command"`
	Count   int64  `json:"count"`
	IsCurl  bool   `json:"isCurl,omitempty"`
}

// TimeBucket is one time-series bucket for stats.
type TimeBucket struct {
	Bucket   string  `json:"bucket"`
	Events   int64   `json:"events"`
	ToolUses int64   `json:"toolUses,omitempty"`
	TokensIn int64   `json:"tokensIn,omitempty"`
	CostUSD  float64 `json:"costUsd,omitempty"`
}

// SearchHit is one FTS5 match bound to its event.
type SearchHit struct {
	EventID   int64   `json:"eventId"`
	SessionID string  `json:"sessionId"`
	Seq       int64   `json:"seq"`
	TS        int64   `json:"ts"`
	Event     string  `json:"event"`
	Tool      string  `json:"tool,omitempty"`
	Snippet   string  `json:"snippet"`
	Rank      float64 `json:"rank"`
}

// SearchGroup is one session-grouped search result.
type SearchGroup struct {
	SessionID   string      `json:"sessionId"`
	Project     string      `json:"project,omitempty"`
	FirstPrompt string      `json:"firstPrompt,omitempty"`
	Hits        []SearchHit `json:"hits"`
	BestRank    float64     `json:"bestRank"`
	HitCount    int         `json:"hitCount"`
}

// SearchResult is the full /search response.
type SearchResult struct {
	Query  string        `json:"query"`
	Total  int64         `json:"total"`
	Groups []SearchGroup `json:"groups"`
}
