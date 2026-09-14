package ingest

import (
	"encoding/json"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	"github.com/arthurobo/agentflow/internal/store"
)

// ToIncoming converts a normalized claudelog event into the store row shape.
// It is the single mapping point between the event model and the index.
func ToIncoming(ev *eventmodel.Event, filePath string) store.Incoming {
	inc := store.Incoming{
		SessionID:       ev.SessionID,
		FilePath:        filePath,
		OriginSessionID: ev.OriginSessionID,
		Project:         ev.Project,
		Event:           string(ev.Type),
		Subtype:         ev.Subtype,
		Actor:           ev.Actor,
		UUID:            ev.UUID,
		ParentUUID:      ev.ParentUUID,
		Seq:             ev.Seq,
		TS:              ev.TS,
		ToolName:        ev.ToolName,
		ToolUseID:       ev.ToolUseID,
		ToolInput:       marshalToolInput(ev.ToolInput),
		Content:         ev.Content,
		ThinkingContent: ev.ThinkingContent,
		ContentHash:     ev.ContentHash,
		Oversized:       ev.OversizedContent,
		IsSidechain:     ev.IsSidechain,
		IsMeta:          ev.IsMeta,
		Retracted:       ev.Retracted,
		HasThinking:     ev.HasThinking,
		ThinkingSig:     ev.ThinkingSignature,
		Model:           ev.Model,
		CostUSD:         ev.CostUSD,
		AgentID:         ev.AgentID,
		FileOffset:      ev.FileOffset,
		RawVersion:      ev.RawVersion,
		IsError:         ev.IsError,
		TermReason:      ev.TerminalReason,
		DurationMs:      ev.DurationMs,
		PermDenials:     permissionDenialCount(ev),
		Source:          string(ev.Source),
	}
	if ev.Tokens != nil {
		inc.TokensIn = ev.Tokens.In
		inc.TokensOut = ev.Tokens.Out
		inc.TokCacheRead = ev.Tokens.CacheRead
		inc.TokCacheWr = ev.Tokens.CacheWrite
	}
	return inc
}

func marshalToolInput(in map[string]any) []byte {
	if len(in) == 0 {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	return b
}

// permissionDenialCount length of the result.permissionDenials array.
func permissionDenialCount(ev *eventmodel.Event) int {
	if len(ev.PermissionDenials) == 0 {
		return 0
	}
	var arr []any
	if err := json.Unmarshal(ev.PermissionDenials, &arr); err != nil {
		return 0
	}
	return len(arr)
}
