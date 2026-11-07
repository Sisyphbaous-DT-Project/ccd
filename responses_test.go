package main

// /v1/responses 的用例：流式与同步都只换思维链、下一轮能换回原版、没规则时逐字节透传。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 假上游：主模型与转述官由同一台服务器扮演 ----------

type respUpstream struct {
	mu     sync.Mutex
	recs   []recorded
	stream string // stream=true 时的响应体
	sync   string // 非流式响应体
}

func (f *respUpstream) add(r *http.Request, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, recorded{path: r.URL.Path, body: append([]byte(nil), body...), header: r.Header.Clone()})
}

func (f *respUpstream) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.recs...)
}

func (f *respUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.add(r, body)
	if isTranslatorCall(body) {
		f.note(w, extractExcerpt(body))
		return
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, f.stream)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, f.sync)
}

func (f *respUpstream) note(w http.ResponseWriter, excerpt string) {
	w.Header().Set("Content-Type", "text/event-stream")
	note := noteFor(excerpt)
	runes := []rune(note)
	mid := len(runes) / 2
	for _, part := range []string{string(runes[:mid]), string(runes[mid:])} {
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": part}}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// ---------- 测试装配 ----------

type respRig struct {
	app   *app
	up    *respUpstream
	relay *httptest.Server
}

func newRespRig(t *testing.T, rules []rule) *respRig {
	t.Helper()
	up := &respUpstream{}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	apiSrv := httptest.NewServer(fakeNewAPI{})
	t.Cleanup(apiSrv.Close)

	// 身份、分组目录、转述 key 都从这个模仿 new-api 的 SQLite 库里查（见 newapi_test.go）：
	// sk-testtoken → 普通用户 7 的令牌 42（normal 分组），令牌 99 是管理员的 key，可当转述用。
	dbPath := newTestSQLite(t)
	a, err := newApp(appOptions{
		listen:     "127.0.0.1:0",
		upstream:   upSrv.URL,
		summaryURL: upSrv.URL + "/v1/chat/completions",
		newAPIBase: apiSrv.URL,
		sqlitePath: dbPath,
		stateDir:   t.TempDir(),
		storeTTL:   24 * time.Hour,
		storeMax:   8 << 20,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { a.db.Close() })
	cfg := defaultConfig()
	cfg.Rules = rules
	cfg.normalize()
	if err := a.cfg.save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)
	return &respRig{app: a, up: up, relay: srv}
}

func (r *respRig) post(t *testing.T, body string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.relay.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-testtoken")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

// bigAdv 让整段原文落在一段里，方便和 noteFor 对照。
func bigAdv() *advanced {
	return &advanced{StreamFirst: 4000, StreamStep: 4000, StreamMax: 8000, SyncFirst: 4000, SyncStep: 4000, SyncMax: 8000}
}

// ---------- 响应体构造 ----------

func sseEvent(name string, data map[string]any) string {
	b, _ := json.Marshal(data)
	return "event: " + name + "\ndata: " + string(b) + "\n\n"
}

func buildRespStreamEnding(raw, answer, ending string) string {
	var b strings.Builder
	b.WriteString(sseEvent("response.created", map[string]any{
		"response": map[string]any{"id": "resp_1", "object": "response", "status": "in_progress", "model": "deepseek-v4.1-flash", "output": []any{}},
		"type":     "response.created", "sequence_number": 0,
	}))
	b.WriteString(sseEvent("response.output_item.added", map[string]any{
		"output_index": 0,
		"item":         map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}},
		"type":         "response.output_item.added", "sequence_number": 1,
	}))
	b.WriteString(sseEvent("response.reasoning_summary_part.added", map[string]any{
		"item_id": "rs_1", "output_index": 0, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
		"type": "response.reasoning_summary_part.added", "sequence_number": 2,
	}))
	half := len(raw) / 2
	for i, part := range []string{raw[:half], raw[half:]} {
		b.WriteString(sseEvent("response.reasoning_summary_text.delta", map[string]any{
			"item_id": "rs_1", "output_index": 0, "summary_index": 0,
			"delta": part, "type": "response.reasoning_summary_text.delta", "sequence_number": 3 + i,
		}))
	}
	b.WriteString(sseEvent("response.reasoning_summary_text.done", map[string]any{
		"item_id": "rs_1", "output_index": 0, "summary_index": 0,
		"text": raw, "type": "response.reasoning_summary_text.done", "sequence_number": 5,
	}))
	b.WriteString(sseEvent("response.reasoning_summary_part.done", map[string]any{
		"item_id": "rs_1", "output_index": 0, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": raw},
		"type": "response.reasoning_summary_part.done", "sequence_number": 6,
	}))
	b.WriteString(sseEvent("response.output_item.done", map[string]any{
		"output_index": 0,
		"item":         map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": raw}}},
		"type":         "response.output_item.done", "sequence_number": 7,
	}))
	b.WriteString(sseEvent("response.output_text.delta", map[string]any{
		"output_index": 1, "content_index": 0, "delta": answer,
		"type": "response.output_text.delta", "sequence_number": 8,
	}))
	b.WriteString(sseEvent(ending, map[string]any{
		"response": map[string]any{
			"id": "resp_1", "object": "response", "status": "completed", "model": "deepseek-v4.1-flash",
			"output": []any{
				map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": raw}}},
				map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": answer}}},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 20, "total_tokens": 30},
		},
		"type": ending, "sequence_number": 9,
	}))
	return b.String()
}

func buildRespStream(raw, answer string) string {
	return buildRespStreamEnding(raw, answer, "response.completed")
}

// parseFrames 把 SSE 输出拆成 (event, data) 序列，方便断言。
type respFrame struct{ event, data string }

func parseFrames(out string) []respFrame {
	var frames []respFrame
	for _, block := range strings.Split(out, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f respFrame
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				f.event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
			if strings.HasPrefix(line, "data: ") {
				f.data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			}
		}
		if f.event != "" || f.data != "" {
			frames = append(frames, f)
		}
	}
	return frames
}

func frameField(t *testing.T, f respFrame, path ...string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(f.data), &v); err != nil {
		t.Fatalf("frame %s data not json: %v", f.event, err)
	}
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("frame %s: not object at %s", f.event, p)
		}
		v = m[p]
	}
	s, _ := v.(string)
	return s
}

// ---------- 1. 流式：原版不外发，摘要进 delta / done / completed ----------

func TestResponsesStreamHidesThinking(t *testing.T) {
	raw := "RAW-SECRET-INTERNAL-THINKING-abcdef"
	answer := "ANSWER-TEXT-OK"
	rig := newRespRig(t, withRule(bigAdv()))
	rig.up.stream = buildRespStream(raw, answer)

	out := rig.post(t, `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}],"stream":true,"reasoning":{"effort":"high"}}`)

	if strings.Contains(out, raw) || strings.Contains(out, "RAW-SECRET") {
		t.Fatalf("原版思维链漏出去了:\n%s", out)
	}
	note := noteFor(raw)
	var deltas strings.Builder
	for _, f := range parseFrames(out) {
		if f.event == "response.reasoning_summary_text.delta" {
			deltas.WriteString(frameField(t, f, "delta"))
		}
	}
	if strings.TrimSpace(deltas.String()) != note {
		t.Fatalf("delta 拼起来应等于摘要 %q，实际 %q", note, strings.TrimSpace(deltas.String()))
	}
	for _, f := range parseFrames(out) {
		switch f.event {
		case "response.reasoning_summary_text.done", "response.reasoning_summary_part.done":
			if got := frameField(t, f, "text"); got != "" && strings.TrimSpace(got) != note {
				t.Fatalf("%s 的 text 应是摘要，实际 %q", f.event, got)
			}
		case "response.output_item.done":
			var v map[string]any
			_ = json.Unmarshal([]byte(f.data), &v)
			item := v["item"].(map[string]any)
			sum := item["summary"].([]any)
			if got, _ := sum[0].(map[string]any)["text"].(string); strings.TrimSpace(got) != note {
				t.Fatalf("output_item.done 的 summary 应是摘要，实际 %q", got)
			}
		case "response.completed":
			var v map[string]any
			_ = json.Unmarshal([]byte(f.data), &v)
			resp := v["response"].(map[string]any)
			output := resp["output"].([]any)
			reasoning := output[0].(map[string]any)
			if got, _ := reasoning["summary"].([]any)[0].(map[string]any)["text"].(string); strings.TrimSpace(got) != note {
				t.Fatalf("completed 的 summary 应是摘要，实际 %q", got)
			}
			if got := resp["usage"].(map[string]any)["total_tokens"]; got != float64(30) {
				t.Fatalf("usage 被改动了: %v", got)
			}
		}
	}
	if !strings.Contains(out, answer) {
		t.Fatalf("正文丢了:\n%s", out)
	}
	if !strings.Contains(out, "event: response.completed") {
		t.Fatalf("结束事件丢了:\n%s", out)
	}
}

// ---------- 2. 同步：整段转述 ----------

func TestResponsesNonStreamHidesThinking(t *testing.T) {
	raw := "RAW-SYNC-THINKING"
	answer := "ANSWER-SYNC"
	rig := newRespRig(t, withRule(bigAdv()))
	rig.up.sync = `{"id":"resp_1","object":"response","status":"completed","model":"deepseek-v4.1-flash","output":[` +
		`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"` + raw + `"}]},` +
		`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"` + answer + `"}]}],` +
		`"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`

	out := rig.post(t, `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}],"stream":false}`)
	if strings.Contains(out, raw) {
		t.Fatalf("原版思维链漏出去了:\n%s", out)
	}
	if !strings.Contains(out, noteFor(raw)) {
		t.Fatalf("没有摘要:\n%s", out)
	}
	if !strings.Contains(out, answer) || !strings.Contains(out, `"total_tokens":30`) {
		t.Fatalf("正文或 usage 被改动了:\n%s", out)
	}
}

// ---------- 3. 下一轮换回原版 ----------

func TestResponsesRestoreRoundTrip(t *testing.T) {
	raw := "RAW-ROUND-TRIP-THINKING"
	answer := "ANSWER-ROUND"
	rig := newRespRig(t, withRule(bigAdv()))
	rig.up.stream = buildRespStream(raw, answer)

	first := rig.post(t, `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}],"stream":true}`)
	note := noteFor(raw)
	var shown strings.Builder
	for _, f := range parseFrames(first) {
		if f.event == "response.reasoning_summary_text.delta" {
			shown.WriteString(frameField(t, f, "delta"))
		}
	}
	if strings.TrimSpace(shown.String()) != note {
		t.Fatalf("第一轮摘要不对: %q", strings.TrimSpace(shown.String()))
	}

	rig.up.recs = nil
	secondBody := `{"model":"deepseek-v4.1-flash","input":[` +
		`{"role":"user","content":"hi"},` +
		`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":` + mustJSON(t, note) + `}]},` +
		`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":` + mustJSON(t, answer) + `}]},` +
		`{"role":"user","content":"next"}` +
		`],"stream":true}`
	out := rig.post(t, secondBody)
	if strings.Contains(out, raw) {
		t.Fatalf("第二轮不应把原版直接发给客户端:\n%s", out)
	}

	var seen []byte
	for _, rec := range rig.up.all() {
		if rec.path == "/v1/responses" && !isTranslatorCall(rec.body) {
			seen = rec.body
		}
	}
	if len(seen) == 0 {
		t.Fatal("上游没收到第二轮请求")
	}
	if !strings.Contains(string(seen), raw) {
		t.Fatalf("第二轮没有把摘要换回原版，上游收到:\n%s", seen)
	}
	if strings.Contains(string(seen), note) {
		t.Fatalf("第二轮还把摘要发给了上游:\n%s", seen)
	}
}

// ---------- 4. 没有规则：逐字节透传 ----------

func TestResponsesPassthrough(t *testing.T) {
	raw := "RAW-PASSTHROUGH"
	rig := newRespRig(t, nil)
	rig.up.stream = buildRespStream(raw, "ANSWER")
	out := rig.post(t, `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}],"stream":true}`)
	if out != rig.up.stream {
		t.Fatalf("没有规则时应逐字节透传\n得到:\n%s\n期望:\n%s", out, rig.up.stream)
	}
}

// ---------- 5. 截断（incomplete）也要改写 ----------

func TestResponsesIncompleteHidesThinking(t *testing.T) {
	raw := "RAW-INCOMPLETE-THINKING"
	rig := newRespRig(t, withRule(bigAdv()))
	rig.up.stream = buildRespStreamEnding(raw, "ANSWER-INCOMPLETE", "response.incomplete")
	out := rig.post(t, `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}],"stream":true}`)
	if strings.Contains(out, raw) {
		t.Fatalf("截断帧漏了原版:\n%s", out)
	}
	if !strings.Contains(out, noteFor(raw)) {
		t.Fatalf("没有摘要:\n%s", out)
	}
	if !strings.Contains(out, "ANSWER-INCOMPLETE") {
		t.Fatalf("正文丢了:\n%s", out)
	}
}

// ---------- 6. 中途失败（failed）也要改写 ----------

func TestResponsesFailedHidesThinking(t *testing.T) {
	raw := "RAW-FAILED-THINKING"
	rig := newRespRig(t, withRule(bigAdv()))
	rig.up.stream = buildRespStreamEnding(raw, "ANSWER-FAILED", "response.failed")
	out := rig.post(t, `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}],"stream":true}`)
	if strings.Contains(out, raw) {
		t.Fatalf("失败帧漏了原版:\n%s", out)
	}
	if !strings.Contains(out, noteFor(raw)) {
		t.Fatalf("没有摘要:\n%s", out)
	}
}

// ---------- 7. 只用精确前缀换回，内容前缀不再兜底 ----------

func TestResponsesRestoreNeedsExactPrefix(t *testing.T) {
	raw := "RAW-EXACT-ONLY"
	rig := newRespRig(t, withRule(bigAdv()))
	rig.up.stream = buildRespStream(raw, "ANSWER")

	first := rig.post(t, `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}],"stream":true}`)
	var shown strings.Builder
	for _, f := range parseFrames(first) {
		if f.event == "response.reasoning_summary_text.delta" {
			shown.WriteString(frameField(t, f, "delta"))
		}
	}
	if strings.TrimSpace(shown.String()) == "" {
		t.Fatal("第一轮没有摘要")
	}
	note := shown.String()

	rig.up.recs = nil
	// 同一个开头，但 user 条目多了一个字段：内容前缀（只看 role/content）相同，精确前缀不同。
	second := `{"model":"deepseek-v4.1-flash","input":[` +
		`{"role":"user","content":"hi","name":"x"},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":` + mustJSON(t, note) + `}]},` +
		`{"role":"user","content":"next"}],"stream":true}`
	rig.post(t, second)

	var seen []byte
	for _, rec := range rig.up.all() {
		if rec.path == "/v1/responses" && !isTranslatorCall(rec.body) {
			seen = rec.body
		}
	}
	if len(seen) == 0 {
		t.Fatal("上游没收到第二轮请求")
	}
	if strings.Contains(string(seen), raw) {
		t.Fatalf("内容前缀不该再兜底换回，上游却收到了原版:\n%s", seen)
	}
	if !strings.Contains(string(seen), strings.TrimSpace(note)) {
		t.Fatalf("摘要应该原样转发给上游:\n%s", seen)
	}
}

// ---------- 8. input 是字符串时不写存储 ----------

func TestResponsesStringInputNotStored(t *testing.T) {
	raw := "RAW-STRING-INPUT"
	rig := newRespRig(t, withRule(bigAdv()))
	rig.up.stream = buildRespStream(raw, "ANSWER")
	out := rig.post(t, `{"model":"deepseek-v4.1-flash","input":"hi","stream":true}`)
	if strings.Contains(out, raw) {
		t.Fatalf("原版漏了:\n%s", out)
	}
	if rig.app.store.Has(7) {
		t.Fatal("input 是字符串时不该写存储（没有可用前缀，会挤同一个键）")
	}
}
