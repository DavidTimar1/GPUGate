package console

import (
	"encoding/json"
	"fmt"
	"strings"
)

// systemContentBlocks turns an Anthropic-style content value (a plain string, an array of content
// blocks, or absent) into a slice of content blocks, so a lifted message's content and the
// request's top-level system field can be merged into one consistent shape.
func systemContentBlocks(content any) []any {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		return v
	default:
		return nil
	}
}

// normalizeMessagesForLlamaCpp lifts every "system" and "developer" message out of the body's
// "messages" array and merges each one, in the order it appeared, into the request's top-level
// "system" field, ahead of whatever that field already holds; the remaining messages keep their
// order. A body with no system or developer message inside "messages" is returned unchanged.
//
// It exists because llama.cpp's Anthropic-to-OpenAI converter
// (server_chat_convert_anthropic_to_oai, the pinned build) turns the top-level "system" field
// into one leading system message and then appends the caller's "messages" verbatim, roles
// included, and the Qwen chat template refuses any system message that is not part of that one
// leading contiguous block ("System message must be at the beginning"). It is pure and never
// sees the request's model name or key.
func normalizeMessagesForLlamaCpp(body []byte) ([]byte, bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, false
	}
	msgs, ok := parsed["messages"].([]any)
	if !ok {
		return body, false
	}
	var lifted []any // each lifted message's own "content" value, in the order it appeared
	remaining := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, isObject := m.(map[string]any)
		if !isObject {
			remaining = append(remaining, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			lifted = append(lifted, mm["content"])
			continue
		}
		remaining = append(remaining, m)
	}
	if len(lifted) == 0 {
		return body, false
	}
	var blocks []any
	for _, content := range lifted {
		blocks = append(blocks, systemContentBlocks(content)...)
	}
	blocks = append(blocks, systemContentBlocks(parsed["system"])...)
	parsed["messages"] = remaining
	if len(blocks) > 0 {
		parsed["system"] = blocks
	} else {
		delete(parsed, "system")
	}
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		// Cannot happen for a value that came from json.Unmarshal, but never forward a body we
		// failed to re-encode as if it were the caller's own.
		return body, false
	}
	return rewritten, true
}

// describeRoleOrder describes an Anthropic-style request body's role order and its top-level
// "system" field, for the one log line per machine that shows what a client sends. Roles only,
// never content or any other field. ok is false when the body is not a JSON object.
func describeRoleOrder(body []byte) (desc string, ok bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", false
	}
	msgs, _ := parsed["messages"].([]any)
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, isObject := m.(map[string]any)
		if !isObject {
			roles = append(roles, "?")
			continue
		}
		role, _ := mm["role"].(string)
		if role == "" {
			role = "?"
		}
		roles = append(roles, role)
	}
	sysDesc := "absent"
	if sys, present := parsed["system"]; present {
		sysDesc = fmt.Sprintf("present, %d block(s)", len(systemContentBlocks(sys)))
	}
	return fmt.Sprintf("roles %s; top-level system %s", strings.Join(roles, ","), sysDesc), true
}

// injectStreamOptionsIncludeUsage adds {"stream_options":{"include_usage":true}} to a streamed
// /v1/chat/completions request that does not already ask for it, otherwise unchanged. Neither
// model server sends a usage chunk on that endpoint's stream unless the caller asks for one
// (`should_include_usage` on vLLM, `stream_options.include_usage` on llama.cpp, both the plain
// OpenAI-spec gate). A non-streamed request, or one already carrying the flag, is returned
// unchanged. A body that is not a JSON object is returned unchanged too; the request reaches the
// machine exactly as sent and earns no usage count.
func injectStreamOptionsIncludeUsage(body []byte) ([]byte, bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, false
	}
	stream, _ := parsed["stream"].(bool)
	if !stream {
		return body, false
	}
	opts, _ := parsed["stream_options"].(map[string]any)
	if opts != nil {
		if include, ok := opts["include_usage"].(bool); ok && include {
			return body, false
		}
	} else {
		opts = map[string]any{}
	}
	opts["include_usage"] = true
	parsed["stream_options"] = opts
	rewritten, err := json.Marshal(parsed)
	if err != nil {
		return body, false
	}
	return rewritten, true
}
