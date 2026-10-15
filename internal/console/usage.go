package console

import (
	"bytes"
	"encoding/json"
)

// usageFields is one usage object's token counts, in whichever of the two shapes it carries:
// Anthropic's input_tokens, output_tokens and cache_read_input_tokens, or OpenAI's
// prompt_tokens, completion_tokens and prompt_tokens_details.cached_tokens. A field this object
// does not carry is left 0; the caller decides whether 0 means "answered zero" or "not present
// here".
type usageFields struct {
	in, out, cached int
}

// numberField reads key off m as a whole number, answering false when the key is absent, null, or
// not a JSON number, so a real 0 and "not present" can be told apart.
func numberField(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64) // encoding/json always decodes a JSON number into float64 here.
	if !ok {
		return 0, false
	}
	return int(f), true
}

// usageFromFields reads one usage object's fields, by whichever shape it carries. OpenAI's
// prompt_tokens already counts every input token including the cached ones, so tokens "in" there
// is prompt_tokens minus cached_tokens; Anthropic's input_tokens already excludes
// cache_read_input_tokens (total_input = input + cache_read + cache_creation), so it is taken
// as is.
func usageFromFields(u map[string]any) usageFields {
	var c usageFields
	if promptTokens, ok := numberField(u, "prompt_tokens"); ok {
		cached := 0
		if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
			cached, _ = numberField(details, "cached_tokens")
		}
		c.in, c.cached = promptTokens-cached, cached
	} else if inputTokens, ok := numberField(u, "input_tokens"); ok {
		c.in = inputTokens
		if cr, ok := numberField(u, "cache_read_input_tokens"); ok {
			c.cached = cr
		}
	}
	if v, ok := numberField(u, "completion_tokens"); ok {
		c.out = v
	} else if v, ok := numberField(u, "output_tokens"); ok {
		c.out = v
	}
	return c
}

// usageScannerMaxBuffer bounds how much of a streamed answer's not-yet-newline-terminated tail,
// or a non-streamed answer's whole body, usageScanner keeps in memory. A call past it counts
// nothing: a stream has no fixed end until [DONE] or message_stop, so this stops an unbounded
// accumulation.
const usageScannerMaxBuffer = 8 << 20 // 8 MB

// usageScanner reads vLLM's and llama.cpp's token counts out of a proxied response as it streams
// past, without changing what the caller receives or when: handleProjectProxy writes and flushes
// every chunk and also feeds a copy of it here. It is only built for a status-200 answer, because
// an error body has no usage field in either model server's shape.
type usageScanner struct {
	streaming       bool
	messagesPath    bool // /v1/messages: input/cache figures live only in message_start
	buf             []byte
	haveInput       bool // message_start has already been seen, on a streamed /v1/messages answer
	in, out, cached int
	// failed is set only on a real parse failure this call's usage could not survive (the buffer
	// overflowed, or a non-streamed body never parsed as JSON at all), never on one bad SSE line
	// among many good ones, and never on a call that carried no usage field.
	failed bool
}

func newUsageScanner(streaming bool, upstreamPath string) *usageScanner {
	return &usageScanner{streaming: streaming, messagesPath: upstreamPath == "/v1/messages"}
}

// feed appends one chunk read from upstream. A streamed answer is scanned line by line as
// complete lines arrive, because a 4096-byte read has no relationship to an SSE event boundary (a
// chunk boundary can split a data: line); the tail of an incomplete line stays in buf until the
// next feed completes it. A non-streamed answer only accumulates; finish parses it whole.
func (u *usageScanner) feed(chunk []byte) {
	if u.failed {
		return
	}
	if len(u.buf)+len(chunk) > usageScannerMaxBuffer {
		u.failed = true
		u.buf = nil
		return
	}
	u.buf = append(u.buf, chunk...)
	if !u.streaming {
		return
	}
	for {
		i := bytes.IndexByte(u.buf, '\n')
		if i < 0 {
			break
		}
		line := u.buf[:i]
		u.buf = u.buf[i+1:]
		u.consumeLine(line)
	}
}

// consumeLine reads one SSE line: everything that is not a "data: " line (a blank line, an
// "event:" line, a comment) is skipped, and "data: [DONE]" carries no JSON. A line whose payload
// fails to parse is skipped too: one bad line never fails the whole scan, since the next event
// may still parse cleanly.
func (u *usageScanner) consumeLine(line []byte) {
	line = bytes.TrimSpace(line)
	const prefix = "data: "
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := line[len(prefix):]
	if string(payload) == "[DONE]" {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return
	}
	u.applyEvent(obj)
}

// applyEvent reads one JSON object's usage, by shape: Anthropic's streamed message_start (input
// and cache figures, nested under "message") and message_delta (output tokens only; vLLM's
// message_delta repeats the input and cache figures, llama.cpp's does not, so only message_start
// is trusted for them); everything else, a non-streamed answer of either model server's shape or
// a streamed OpenAI-style final usage chunk, carries its usage at the top level.
func (u *usageScanner) applyEvent(obj map[string]any) {
	if t, _ := obj["type"].(string); t == "message_start" {
		if msg, ok := obj["message"].(map[string]any); ok {
			if usage, ok := msg["usage"].(map[string]any); ok {
				c := usageFromFields(usage)
				u.in, u.cached = c.in, c.cached
				u.haveInput = true
			}
		}
		return
	}
	if t, _ := obj["type"].(string); t == "message_delta" {
		if usage, ok := obj["usage"].(map[string]any); ok {
			if v, ok := numberField(usage, "output_tokens"); ok {
				u.out = v
			}
		}
		return
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		c := usageFromFields(usage)
		if !u.messagesPath || !u.haveInput {
			u.in, u.cached = c.in, c.cached
		}
		u.out = c.out
	}
}

// finish answers the totals this call's usage scan found. A streamed answer has already consumed
// every complete line through feed; a non-streamed answer is parsed whole here, once. ok is false
// when nothing was found at all (the buffer overflowed, the body never parsed, or the call
// carried no usage, a bodyless GET for instance), so the caller counts nothing for it rather than
// writing three zeros over a real total.
func (u *usageScanner) finish() (in, out, cached int, ok bool) {
	if u.failed {
		return 0, 0, 0, false
	}
	if !u.streaming && len(u.buf) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(u.buf, &obj); err != nil {
			u.failed = true
			return 0, 0, 0, false
		}
		u.applyEvent(obj)
	}
	if u.in == 0 && u.out == 0 && u.cached == 0 {
		return 0, 0, 0, false
	}
	return u.in, u.out, u.cached, true
}
