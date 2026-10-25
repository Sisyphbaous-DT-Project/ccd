package thinkhide

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// WrapSnippet 把待转述的思考片段包起来，明确它只是材料而不是对转述官说的话。
func WrapSnippet(text string) string {
	return "<思考片段>\n" + text + "\n</思考片段>\n\n只输出那一两句转述。"
}

func clipSnippet(text string) string {
	if r := []rune(text); len(r) > 6000 {
		return string(r[:4500]) + "\n……\n" + string(r[len(r)-1500:])
	}
	return text
}

// HTTPTranslator 通过 OpenAI 兼容的 chat/completions 流式接口做转述。
type HTTPTranslator struct {
	Client      *http.Client
	URL         string
	Header      http.Header
	Model       string
	System      string
	Shots       [][2]string                                   // few-shot：思考片段 → 转述
	Wrap        func(ctx context.Context, text string) string // 把片段包成用户消息（ctx 里有 SegmentIndex/ReplySeed）；nil 用 WrapSnippet
	StrictNote  string                                        // 重试时追加在用户消息末尾的更严格的提醒
	Temperature float64
	MaxTokens   int
	Extra       map[string]any // 额外请求字段，例如 reasoning_effort=off
}

// Translate 实现 Translator。
func (h *HTTPTranslator) Translate(ctx context.Context, segment string, strict bool, emit func(string) bool) error {
	msgs := []any{map[string]any{"role": "system", "content": h.System}}
	wrap := h.Wrap
	if wrap == nil {
		wrap = func(_ context.Context, s string) string { return WrapSnippet(s) }
	}
	for _, shot := range h.Shots {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": WrapSnippet(shot[0])},
			map[string]any{"role": "assistant", "content": shot[1]})
	}
	user := wrap(ctx, clipSnippet(segment))
	if strict && h.StrictNote != "" {
		user += "\n\n" + h.StrictNote
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": user})
	body := map[string]any{
		"model":       h.Model,
		"messages":    msgs,
		"stream":      true,
		"max_tokens":  h.MaxTokens,
		"temperature": h.Temperature,
	}
	for k, v := range h.Extra {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	for k, vs := range h.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return fmt.Errorf("translator upstream status %d", resp.StatusCode)
	}
	return StreamContentDeltas(ctx, resp.Body, emit)
}
