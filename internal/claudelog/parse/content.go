package parse

import (
	"strings"

	"encoding/json"

	gojson "github.com/goccy/go-json"
)

// contentBlock is a normalized view of one content block inside a raw
// user/assistant message ( rule 2: tools are content blocks, not
// top-level record types).
type contentBlock struct {
	kind              string // text | thinking | tool_use | tool_result | image | other
	text              string
	thinkingSignature string
	toolName          string
	toolInput         map[string]any
	toolUseID         string
}

// decodeBlocks turns raw content JSON (string or array of blocks) into
// contentBlock values. Unknown block types are preserved as kind "other" (their
// bytes remain in the event's Raw) so nothing is lost.
func decodeBlocks(contentRaw json.RawMessage) []contentBlock {
	var s string
	if err := gojson.Unmarshal(contentRaw, &s); err == nil {
		return []contentBlock{{kind: "text", text: s}}
	}
	var arr []json.RawMessage
	if err := gojson.Unmarshal(contentRaw, &arr); err != nil {
		return nil
	}
	var blocks []contentBlock
	for _, b := range arr {
		var bm map[string]json.RawMessage
		if err := gojson.Unmarshal(b, &bm); err != nil {
			continue
		}
		var blk contentBlock
		switch jsonStr(bm["type"]) {
		case "text":
			blk.kind, blk.text = "text", jsonStr(bm["text"])
		case "thinking":
			blk.kind = "thinking"
			blk.text = jsonStr(bm["thinking"])
			blk.thinkingSignature = jsonStr(bm["signature"])
		case "tool_use":
			blk.kind = "tool_use"
			blk.toolName = jsonStr(bm["name"])
			blk.toolInput = jsonObj(bm["input"])
			blk.toolUseID = jsonStr(bm["id"])
		case "tool_result":
			blk.kind = "tool_result"
			blk.toolUseID = jsonStr(bm["tool_use_id"])
			blk.toolName = jsonStr(bm["tool_name"])
			blk.text = toolResultText(bm)
		case "image":
			blk.kind = "image"
		default:
			blk.kind = "other"
		}
		blocks = append(blocks, blk)
	}
	return blocks
}

// toolResultFromObject builds a tool_result block from a top-level
// `tool_use_result` object (a stream-json shape). It also unwraps the common
// `file.content` form so the readable text survives normalization.
func toolResultFromObject(raw json.RawMessage) *contentBlock {
	var m map[string]json.RawMessage
	if err := gojson.Unmarshal(raw, &m); err != nil {
		return nil
	}
	b := &contentBlock{
		kind:      "tool_result",
		toolUseID: jsonStr(m["tool_use_id"]),
		toolName:  jsonStr(m["tool_name"]),
		text:      toolResultText(m),
	}
	if b.text == "" {
		// { "type":"text", "file": { "content": "..." } }
		if file := jsonObj(m["file"]); file != nil {
			if c, ok := file["content"].(string); ok {
				b.text = c
			}
		}
	}
	return b
}

// toolResultText extracts readable text from a tool_result block's `content`
// member (string or array of text blocks).
func toolResultText(bm map[string]json.RawMessage) string {
	c, ok := bm["content"]
	if !ok || c == nil {
		return ""
	}
	var parts []string
	for _, sub := range decodeBlocks(c) {
		if sub.kind == "text" {
			parts = append(parts, sub.text)
		}
	}
	s := strings.Join(parts, "\n")
	if s != "" {
		return s
	}
	// Content that is neither a string nor a block array (e.g. a raw object)
	// is left empty here; its bytes persist in the event Raw.
	return ""
}

// jsonStr decodes a RawMessage into a string, tolerantly returning "".
func jsonStr(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := gojson.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func jsonObj(raw json.RawMessage) map[string]any {
	if raw == nil {
		return nil
	}
	var m map[string]any
	if err := gojson.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}
