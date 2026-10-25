package main

// 只测这次改动本身：透传的逐字节一致、四种格式的思维链替换、多块穿插工具调用、
// 下一轮换回原版（含重启后仍能换回）、转述失败不出固定话术、三套提示词、auto 分组匹配。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sisyphbaous-DT-Project/ccd/internal/thinkhide"
)

// ---------- 假上游：同一台服务器既扮演「主模型」又扮演「转述官」 ----------

type recorded struct {
	path   string
	body   []byte
	header http.Header
}

type fakeUpstream struct {
	mu       sync.Mutex
	recs     []recorded
	chatBody func(model string, original string, stream bool) string
	msgBody  func(model string, blocks []fakeBlock, stream bool) string
	failNote bool
}

type fakeBlock struct {
	kind      string // thinking | text | redacted | tool_use
	text      string
	signature string
}

func newFakeUpstream() *fakeUpstream { return &fakeUpstream{} }

func (f *fakeUpstream) add(r *http.Request, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, recorded{path: r.URL.Path, body: append([]byte(nil), body...), header: r.Header.Clone()})
}

func (f *fakeUpstream) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.recs...)
}

func (f *fakeUpstream) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = nil
}

// noteFor 给一段片段生成稳定的「笔记」，测试里只检查它出现了、原版没出现。
func noteFor(excerpt string) string {
	sum := sha256.Sum256([]byte(excerpt))
	return "NOTE-" + hex.EncodeToString(sum[:3]) + " working through the next step"
}

func isTranslatorCall(body []byte) bool {
	return bytes.Contains(body, []byte(noteMarker))
}

func extractExcerpt(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return ""
	}
	c := req.Messages[len(req.Messages)-1].Content
	if i := strings.Index(c, "<thinking_excerpt>"); i >= 0 {
		c = c[i+len("<thinking_excerpt>"):]
	}
	if i := strings.Index(c, "</thinking_excerpt>"); i >= 0 {
		c = c[:i]
	}
	return strings.TrimSpace(c)
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.add(r, body)

	if isTranslatorCall(body) {
		f.streamNote(w, extractExcerpt(body))
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	var payload string
	switch r.URL.Path {
	case "/v1/chat/completions":
		payload = f.chatBody(req.Model, "", req.Stream)
	case "/v1/messages":
		payload = f.msgBody(req.Model, nil, req.Stream)
	default:
		w.WriteHeader(404)
		return
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	io.WriteString(w, payload)
}

func (f *fakeUpstream) streamNote(w http.ResponseWriter, excerpt string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	if f.failNote {
		return
	}
	note := noteFor(excerpt)
	r := []rune(note)
	mid := len(r) / 2
	for _, part := range []string{string(r[:mid]), string(r[mid:])} {
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": part}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	io.WriteString(w, "data: [DONE]\n\n")
}

// ---------- 假 new-api 后台（只给管理接口的管理员校验用） ----------

type fakeNewAPI struct{}

func (fakeNewAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/user/self":
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"role": 100, "id": 1}})
	default:
		w.WriteHeader(404)
	}
}

// ---------- 测试脚手架 ----------

type rig struct {
	app    *app
	up     *fakeUpstream
	relay  *httptest.Server
	state  string
	dbPath string
}

func newRig(t *testing.T, rules []rule) *rig {
	t.Helper()
	up := newFakeUpstream()
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	apiSrv := httptest.NewServer(fakeNewAPI{})
	t.Cleanup(apiSrv.Close)

	// 身份、分组目录、转述 key 都从这个模仿 new-api 的 SQLite 库里查（见 newapi_test.go）：
	// sk-testtoken → 普通用户 7 的令牌 42（normal 分组），令牌 99 是管理员的 key，可当转述用。
	dbPath := newTestSQLite(t)
	state := t.TempDir()
	a, err := newApp(appOptions{
		listen:     "127.0.0.1:0",
		upstream:   upSrv.URL,
		summaryURL: upSrv.URL + "/v1/chat/completions",
		newAPIBase: apiSrv.URL,
		sqlitePath: dbPath,
		stateDir:   state,
		storeTTL:   24 * time.Hour,
		storeMax:   8 << 20,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { a.db.Close() })
	if len(rules) > 0 {
		cfg := defaultConfig()
		cfg.Rules = rules
		cfg.normalize()
		if err := a.cfg.save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}
	}
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)
	return &rig{app: a, up: up, relay: srv, state: state, dbPath: dbPath}
}

func (r *rig) post(t *testing.T, path, body string, hdr map[string]string) (string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.relay.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-testtoken")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b), resp.Header
}

func withRule(adv *advanced) []rule {
	return []rule{{
		ID: "r1", Enabled: true, Group: "normal", Lang: langEN,
		TranslatorGroup: "normal", TranslatorTokenID: 99, TranslatorModel: "deepseek-v4.1-flash",
		Advanced: adv,
	}}
}

func smallAdv() *advanced {
	return &advanced{StreamFirst: 40, StreamStep: 20, StreamMax: 200, SyncFirst: 40, SyncStep: 20, SyncMax: 200}
}

// ---------- 1. 没规则的令牌：请求和回复逐字节一致 ----------

func TestPassthroughByteIdentical(t *testing.T) {
	r := newRig(t, nil) // 没有任何规则
	const originalThinking = "SECRET-ORIGINAL-THINKING he is weighing two options carefully"
	reply := chatStreamScript("deepseek-v4.1-flash", originalThinking)
	r.up.chatBody = func(string, string, bool) string { return reply }

	reqBody := `{"model":"deepseek-v4.1-flash",  "messages":[{"role":"assistant","reasoning_content":"keep me","content":"hi"}],"reasoning_effort":"high","thinking":{"type":"enabled","budget_tokens":2048},"stream":true,"temperature":0.7}`
	got, _ := r.post(t, "/v1/chat/completions", reqBody, nil)

	recs := r.up.all()
	if len(recs) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(recs))
	}
	if string(recs[0].body) != reqBody {
		t.Fatalf("request body changed:\n got %s\nwant %s", recs[0].body, reqBody)
	}
	if got != reply {
		t.Fatalf("response changed:\n got %q\nwant %q", got, reply)
	}
}

func chatStreamScript(model, thinking string) string {
	var b strings.Builder
	head := fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":%q,"system_fingerprint":"fp_abc","choices":[`, model)
	emit := func(choice string) {
		b.WriteString("data: ")
		b.WriteString(head)
		b.WriteString(choice)
		b.WriteString("]}\n\n")
	}
	emit(`{"index":0,"delta":{"role":"assistant"},"finish_reason":null}`)
	r := []rune(thinking)
	for i := 0; i < len(r); i += 30 {
		j := i + 30
		if j > len(r) {
			j = len(r)
		}
		enc, _ := json.Marshal(string(r[i:j]))
		emit(`{"index":0,"delta":{"reasoning_content":` + string(enc) + `},"finish_reason":null}`)
	}
	emit(`{"index":0,"delta":{"content":"the answer"},"finish_reason":null}`)
	emit(`{"index":0,"delta":{},"finish_reason":"stop"}`)
	fmt.Fprintf(&b, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33,\"completion_tokens_details\":{\"reasoning_tokens\":17}}}\n\n", model)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// ---------- 2. chat 流式：思维链换成转述，其它帧逐字节不变 ----------

func TestChatStreamHidesThinking(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const original = "SECRET-ORIGINAL-THINKING alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau"
	reply := chatStreamScript("deepseek-v4.1-flash", original)
	r.up.chatBody = func(string, string, bool) string { return reply }

	reqBody := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}],"stream":true}`
	got, hdr := r.post(t, "/v1/chat/completions", reqBody, nil)

	if strings.Contains(got, "SECRET-ORIGINAL") {
		t.Fatalf("original thinking leaked to client: %s", got)
	}
	if !strings.Contains(got, "NOTE-") {
		t.Fatalf("no note emitted: %s", got)
	}
	if !strings.Contains(got, `"content":"the answer"`) {
		t.Fatalf("content lost: %s", got)
	}
	if !strings.Contains(got, `"reasoning_tokens":17`) {
		t.Fatalf("usage lost: %s", got)
	}
	if !strings.Contains(got, `"system_fingerprint":"fp_abc"`) {
		t.Fatalf("system_fingerprint lost: %s", got)
	}
	if hdr.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type changed: %s", hdr.Get("Content-Type"))
	}
	// 摘要必须排在正文之前（先想后答）
	if strings.Index(got, "NOTE-") > strings.Index(got, `"content":"the answer"`) {
		t.Fatalf("note came after content")
	}
	// 上游请求必须逐字节等于客户端发的（没有历史摘要要换回）
	recs := r.up.all()
	if len(recs) < 2 || string(recs[0].body) != reqBody {
		t.Fatalf("upstream request changed: %s", recs[0].body)
	}
	// 转述请求的参数：关思考的两个参数都在
	var tr struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		ReasoningEffort string         `json:"reasoning_effort"`
		Thinking        map[string]any `json:"thinking"`
		MaxTokens       int            `json:"max_tokens"`
	}
	if json.Unmarshal(recs[1].body, &tr) != nil {
		t.Fatalf("translator request not json: %s", recs[1].body)
	}
	if tr.ReasoningEffort != "off" || tr.Thinking["type"] != "disabled" {
		t.Fatalf("translator thinking params missing: %s", recs[0].body)
	}
	if tr.MaxTokens != 400 {
		t.Fatalf("translator max_tokens = %d", tr.MaxTokens)
	}
	if !strings.Contains(tr.Messages[0].Content, noteMarker) {
		t.Fatalf("english system prompt missing")
	}
}

// ---------- 3. chat 同步 ----------

func TestChatNonStreamHidesThinking(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const original = "SECRET-ORIGINAL-THINKING one two three four five six seven eight nine ten eleven twelve"
	r.up.chatBody = func(model, _ string, _ bool) string {
		b, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-2", "object": "chat.completion", "created": 1700000000, "model": model,
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role": "assistant", "content": "final answer", "reasoning_content": original,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
		})
		return string(b)
	}
	got, _ := r.post(t, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"q"}],"stream":false}`, nil)
	if strings.Contains(got, "SECRET-ORIGINAL") {
		t.Fatalf("original leaked: %s", got)
	}
	if !strings.Contains(got, "NOTE-") || !strings.Contains(got, "final answer") {
		t.Fatalf("bad body: %s", got)
	}
	if !strings.Contains(got, `"total_tokens":11`) {
		t.Fatalf("usage lost: %s", got)
	}
}

// ---------- 4. messages：多块穿插工具调用 ----------

func messagesScript(blocks []fakeBlock) string {
	var b strings.Builder
	msgStart := `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`
	fmt.Fprintf(&b, "event: message_start\ndata: %s\n\n", msgStart)
	fmt.Fprintf(&b, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	for i, blk := range blocks {
		switch blk.kind {
		case "thinking":
			fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n", i)
			r := []rune(blk.text)
			for k := 0; k < len(r); k += 25 {
				j := k + 25
				if j > len(r) {
					j = len(r)
				}
				enc, _ := json.Marshal(string(r[k:j]))
				fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":%s}}\n\n", i, string(enc))
			}
			if blk.signature != "" {
				enc, _ := json.Marshal(blk.signature)
				fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"signature_delta\",\"signature\":%s}}\n\n", i, string(enc))
			}
		case "redacted":
			fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"ENCRYPTED\"}}\n\n", i)
		case "text":
			fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", i)
			enc, _ := json.Marshal(blk.text)
			fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", i, string(enc))
		case "tool_use":
			fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n", i)
			fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\n", i)
		}
		fmt.Fprintf(&b, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
	}
	fmt.Fprintf(&b, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":42}}\n\n")
	fmt.Fprintf(&b, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return b.String()
}

func TestMessagesStreamMultiBlock(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const think1 = "SECRET-BLOCK-ONE thinking about the first subproblem with numbers 1071 and 462 and the gcd"
	const think2 = "SECRET-BLOCK-TWO after the tool result, we need to format the answer for the user neatly"
	script := messagesScript([]fakeBlock{
		{kind: "thinking", text: think1, signature: "SIG-ONE-abcdef"},
		{kind: "text", text: "Let me look that up."},
		{kind: "tool_use"},
		{kind: "thinking", text: think2, signature: "SIG-TWO-uvwxyz"},
		{kind: "text", text: "Done."},
	})
	r.up.msgBody = func(string, []fakeBlock, bool) string { return script }

	got, _ := r.post(t, "/v1/messages", `{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"stream":true}`, map[string]string{"x-api-key": "sk-testtoken"})

	if strings.Contains(got, "SECRET-BLOCK") {
		t.Fatalf("original thinking leaked:\n%s", got)
	}
	if !strings.Contains(got, "NOTE-") {
		t.Fatalf("no notes emitted:\n%s", got)
	}
	// 签名必须原样保留，且在两块里各出现一次
	if strings.Count(got, `"signature":"SIG-ONE-abcdef"`) != 1 || strings.Count(got, `"signature":"SIG-TWO-uvwxyz"`) != 1 {
		t.Fatalf("signature changed:\n%s", got)
	}
	// 块序号不能变
	for _, idx := range []string{`"index":0`, `"index":1`, `"index":2`, `"index":3`, `"index":4`} {
		if !strings.Contains(got, idx) {
			t.Fatalf("index %s missing:\n%s", idx, got)
		}
	}
	// redacted 块在别的用例里检查；这里检查文本块与事件顺序
	if !strings.Contains(got, `"text":"Let me look that up."`) || !strings.Contains(got, `"text":"Done."`) {
		t.Fatalf("text blocks lost:\n%s", got)
	}
	if !strings.Contains(got, `"type":"tool_use"`) || !strings.Contains(got, `"output_tokens":42`) {
		t.Fatalf("tool_use or usage lost:\n%s", got)
	}
	// 摘要必须在该块 signature_delta 之前
	if strings.Index(got, "NOTE-") > strings.Index(got, "SIG-ONE-abcdef") {
		t.Fatalf("notes came after signature_delta")
	}
	// 思维链块必须在后面的 text 块之前结束：NOTE 全部出现在 "Let me look that up." 之前
	firstNote := strings.Index(got, "NOTE-")
	if firstNote > strings.Index(got, "Let me look that up.") {
		t.Fatalf("notes came after the text block")
	}
}

func TestMessagesRedactedUntouched(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	script := messagesScript([]fakeBlock{
		{kind: "thinking", text: "SECRET-THINKING-xxx with enough words to make a segment", signature: "SIG-1"},
		{kind: "redacted"},
		{kind: "text", text: "ok"},
	})
	r.up.msgBody = func(string, []fakeBlock, bool) string { return script }
	got, _ := r.post(t, "/v1/messages", `{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"stream":true}`, map[string]string{"x-api-key": "sk-testtoken"})
	if !strings.Contains(got, `{"type":"redacted_thinking","data":"ENCRYPTED"}`) {
		t.Fatalf("redacted block changed:\n%s", got)
	}
	if strings.Contains(got, "SECRET-THINKING") {
		t.Fatalf("thinking leaked:\n%s", got)
	}
}

// ---------- 5. messages 同步 ----------

func TestMessagesNonStream(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const think = "SECRET-SYNC-THINKING with plenty of words to summarise here and there"
	r.up.msgBody = func(model string, _ []fakeBlock, _ bool) string {
		b, _ := json.Marshal(map[string]any{
			"id": "msg_2", "type": "message", "role": "assistant", "model": model,
			"content": []any{
				map[string]any{"type": "thinking", "thinking": think, "signature": "SIG-SYNC"},
				map[string]any{"type": "text", "text": "answer"},
			},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 3, "output_tokens": 4},
		})
		return string(b)
	}
	got, _ := r.post(t, "/v1/messages", `{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"stream":false}`, map[string]string{"x-api-key": "sk-testtoken"})
	if strings.Contains(got, "SECRET-SYNC") {
		t.Fatalf("original leaked: %s", got)
	}
	if !strings.Contains(got, "NOTE-") || !strings.Contains(got, `"signature":"SIG-SYNC"`) {
		t.Fatalf("bad body: %s", got)
	}
}

// ---------- 6. 下一轮换回原版（含重启后仍能换回） ----------

func TestRestoreChatRoundTrip(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const original = "ORIGINAL-THINKING about the plan: step one, step two, step three, and a final check of the numbers"
	reply := chatStreamScript("deepseek-v4.1-flash", original)
	r.up.chatBody = func(string, string, bool) string { return reply }

	req1 := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}],"stream":true}`
	got1, _ := r.post(t, "/v1/chat/completions", req1, nil)
	shown := collectReasoning(t, got1)
	if shown == "" {
		t.Fatalf("no reasoning emitted:\n%s", got1)
	}

	enc, _ := json.Marshal(shown)
	req2 := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"},{"role":"assistant","reasoning_content":%s,"content":"the answer"},{"role":"user","content":"next"}],"stream":true}`, enc)
	wantBody := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"},{"role":"assistant","reasoning_content":%s,"content":"the answer"},{"role":"user","content":"next"}],"stream":true}`, mustJSON(t, original))

	r.up.reset()
	r.post(t, "/v1/chat/completions", req2, nil)
	recs := r.up.all()
	if len(recs) == 0 || string(recs[0].body) != wantBody {
		t.Fatalf("restored request mismatch:\n got %s\nwant %s", recs[0].body, wantBody)
	}
}

func TestRestoreSurvivesRestart(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const original = "ORIGINAL-THINKING survives a restart, with several words so a segment is produced"
	reply := chatStreamScript("deepseek-v4.1-flash", original)
	r.up.chatBody = func(string, string, bool) string { return reply }

	got1, _ := r.post(t, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	shown := collectReasoning(t, got1)
	if shown == "" {
		t.Fatalf("no reasoning emitted")
	}

	// 重新打开同一份状态目录（等价于服务重启）
	a2, err := newApp(appOptions{
		listen: "127.0.0.1:0", upstream: r.app.upstream.String(),
		summaryURL: r.app.summaryURL, newAPIBase: r.app.apiBase, sqlitePath: r.dbPath,
		stateDir: r.state, storeTTL: 24 * time.Hour, storeMax: 8 << 20,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { a2.db.Close() })
	if !a2.store.Has(7) {
		t.Fatalf("store did not remember the user after reopen")
	}
	req2 := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":%s,"content":"x"}],"stream":false}`,
		mustJSON(t, shown))
	out, _, _, restored := prepareChat([]byte(req2), 7, a2.store)
	if restored != 1 {
		t.Fatalf("restored %d blocks after reopen, want 1", restored)
	}
	if !bytes.Contains(out, []byte(original)) {
		t.Fatalf("original not restored: %s", out)
	}
}

func collectReasoning(t *testing.T, sse string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &ch) != nil {
			continue
		}
		if len(ch.Choices) > 0 {
			sb.WriteString(ch.Choices[0].Delta.ReasoningContent)
		}
	}
	return sb.String()
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ---------- 7. 转述失败：不出固定话术，正文照常 ----------

func TestTranslatorFailureNoCannedText(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	r.up.failNote = true
	const original = "SECRET-ORIGINAL-THINKING that must never be shown to the client, absolutely never"
	reply := chatStreamScript("deepseek-v4.1-flash", original)
	r.up.chatBody = func(string, string, bool) string { return reply }

	got, _ := r.post(t, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if strings.Contains(got, "SECRET-ORIGINAL") {
		t.Fatalf("original leaked:\n%s", got)
	}
	// 允许那条只有一个空格的 keepalive（首字时间补偿），但不能出现任何文字
	if reasoning := strings.TrimSpace(collectReasoning(t, got)); reasoning != "" {
		t.Fatalf("translator failure must not emit any text, got %q", reasoning)
	}
	if !strings.Contains(got, `"content":"the answer"`) {
		t.Fatalf("content lost:\n%s", got)
	}
	if strings.Contains(got, "NOTE-") {
		t.Fatalf("unexpected note:\n%s", got)
	}
}

// ---------- 8. 三套提示词 ----------

func TestPromptsByLanguage(t *testing.T) {
	en := promptFor(langEN, "")
	if !strings.Contains(en.System, noteMarker) || en.MaxRunes != 420 {
		t.Fatalf("english prompt wrong")
	}
	zh := promptFor(langZH, "")
	if !strings.Contains(zh.System, "工作笔记") || !zh.Chinese || zh.MaxRunes != 200 {
		t.Fatalf("chinese prompt wrong")
	}
	if len(zh.Avoid) == 0 || zh.Avoid[0] != "用户" {
		t.Fatalf("chinese avoid prefix missing")
	}
	custom := promptFor(langCustom, "日语")
	if custom.LangLabel != "日语" {
		t.Fatalf("custom lang missing")
	}
	wrapped := customWrap(custom, "abc")
	if !strings.Contains(wrapped, "Write the whole note in 日语") || !strings.Contains(wrapped, "but say it in 日语") {
		t.Fatalf("custom language not embedded: %s", wrapped)
	}
	if !strings.Contains(custom.System, "including its opening words, must be in 日语") {
		t.Fatalf("custom system prompt should require the opening in the target language too")
	}
}

func customWrap(ps promptSet, text string) string {
	ctx := thinkhide.WithSegment(context.Background(), 0, 3)
	return makeWrap(ps, &recentNotes{})(ctx, text)
}

// ---------- 9. auto 分组 ----------

func TestAutoGroupResolve(t *testing.T) {
	cat := &catalog{Groups: []catalogGroup{
		{Name: "vip", Models: []string{"a-model"}},
		{Name: "normal", Models: []string{"deepseek-v4.1-flash"}},
	}}
	cat.build()
	id := identity{TokenGroup: "auto", AutoGroups: []string{"vip", "normal"}}
	if got := resolveGroup(id, "deepseek-v4.1-flash", cat); got != "normal" {
		t.Fatalf("resolveGroup = %q, want normal", got)
	}
	if got := resolveGroup(id, "unknown", cat); got != "" {
		t.Fatalf("resolveGroup for unknown model = %q", got)
	}
	id2 := identity{TokenGroup: "claude"}
	if got := resolveGroup(id2, "whatever", cat); got != "claude" {
		t.Fatalf("non-auto group = %q", got)
	}
}

func TestRuleMatchPriority(t *testing.T) {
	cfg := defaultConfig()
	cfg.Rules = []rule{
		{ID: "whole", Enabled: true, Group: "normal", Lang: langEN, TranslatorGroup: "normal", TranslatorTokenID: 99, TranslatorModel: "m"},
		{ID: "one", Enabled: true, Group: "normal", Models: []string{"special"}, Lang: langEN, TranslatorGroup: "normal", TranslatorTokenID: 99, TranslatorModel: "m"},
	}
	cfg.normalize()
	if got := cfg.match("normal", "special"); got == nil || got.ID != "one" {
		t.Fatalf("model rule should win: %+v", got)
	}
	if got := cfg.match("normal", "other"); got == nil || got.ID != "whole" {
		t.Fatalf("whole group rule expected: %+v", got)
	}
	if got := cfg.match("claude", "special"); got != nil {
		t.Fatalf("no rule expected for claude")
	}
}

// ---------- 9b. 首字时间：第一条被吞掉的思维链帧要换成空 delta 放出去 ----------

func TestChatFirstFrameKeepsTTFT(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const original = "SECRET-ORIGINAL-THINKING the very first frame carries only thinking and must not leak"
	var b strings.Builder
	head := `{"id":"chatcmpl-9","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4.1-flash","choices":[`
	r0 := []rune(original)
	for i := 0; i < len(r0); i += 40 {
		j := i + 40
		if j > len(r0) {
			j = len(r0)
		}
		enc, _ := json.Marshal(string(r0[i:j]))
		b.WriteString("data: " + head + `{"index":0,"delta":{"reasoning_content":` + string(enc) + `},"finish_reason":null}]}` + "\n\n")
	}
	b.WriteString("data: " + head + `{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	reply := b.String()
	r.up.chatBody = func(string, string, bool) string { return reply }

	got, _ := r.post(t, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"q"}],"stream":true}`, nil)
	first := ""
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "data: [DONE]") {
			first = strings.TrimSpace(line[5:])
			break
		}
	}
	if first == "" {
		t.Fatalf("no data frame at all")
	}
	if strings.Contains(first, "SECRET-ORIGINAL") {
		t.Fatalf("first frame leaked thinking: %s", first)
	}
	if !strings.Contains(first, `"reasoning_content":" "`) {
		t.Fatalf("first frame should be a single-space keepalive, got %s", first)
	}
	if !strings.Contains(first, `"id":"chatcmpl-9"`) {
		t.Fatalf("first frame lost the upstream envelope: %s", first)
	}
}

// ---------- 10. 请求体不是 JSON 时原样透传 ----------

func TestBrokenJSONPassthrough(t *testing.T) {
	// 规则只认某个具体模型：模型读不出来就不该命中，请求和回复都原样。
	r := newRig(t, []rule{{
		ID: "r1", Enabled: true, Group: "normal", Models: []string{"special-model"}, Lang: langEN,
		TranslatorGroup: "normal", TranslatorTokenID: 99, TranslatorModel: "deepseek-v4.1-flash",
	}})
	reply := chatStreamScript("deepseek-v4.1-flash", "SECRET hidden thinking that stays")
	r.up.chatBody = func(string, string, bool) string { return reply }
	const bad = `{"model":"deepseek-v4.1-flash","messages":[` // 故意坏掉
	got, _ := r.post(t, "/v1/chat/completions", bad, nil)
	recs := r.up.all()
	if len(recs) != 1 || string(recs[0].body) != bad {
		t.Fatalf("broken body not passed through: %s", recs[0].body)
	}
	if got != reply {
		t.Fatalf("response should be untouched")
	}
}

// ---------- 审查补充：转发头、用量帧、按 key 套转述 ----------

func TestForwardedHeadersKept(t *testing.T) {
	r := newRig(t, nil)
	r.up.chatBody = func(string, string, bool) string { return `{"id":"x","choices":[]}` }
	hdr := map[string]string{
		"X-Forwarded-For":   "203.0.113.9, 198.51.100.7",
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "api.example.test",
		"Forwarded":         "for=203.0.113.9;proto=https",
		"X-Real-Ip":         "198.51.100.7",
	}
	r.post(t, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","messages":[]}`, hdr)
	recs := r.up.all()
	if len(recs) != 1 {
		t.Fatalf("upstream got %d requests, want 1", len(recs))
	}
	for k, v := range hdr {
		if got := recs[0].header.Get(k); got != v {
			t.Fatalf("header %s = %q, want %q", k, got, v)
		}
	}
}

func TestChatUsageFrameStripsThinking(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const secret = "SECRET-ORIGINAL-THINKING rides in frames that also carry a usage key, and some upstreams write it twice"
	head := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"deepseek-v4.1-flash","usage":null,"choices":[`
	var b strings.Builder
	for _, part := range []string{secret[:50], secret[50:]} {
		enc, _ := json.Marshal(part)
		b.WriteString("data: " + head + `{"index":0,"delta":{"reasoning_content":` + string(enc) + `,"reasoning":` + string(enc) + `},"finish_reason":null}]}` + "\n\n")
	}
	b.WriteString("data: " + head + `{"index":0,"delta":{"content":"the answer"},"finish_reason":"stop"}]}` + "\n\n")
	b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"deepseek-v4.1-flash","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	reply := b.String()
	r.up.chatBody = func(string, string, bool) string { return reply }

	got, _ := r.post(t, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if strings.Contains(got, "SECRET-ORIGINAL") {
		t.Fatalf("original thinking leaked: %s", got)
	}
	if !strings.Contains(got, "NOTE-") || !strings.Contains(got, `"content":"the answer"`) || !strings.Contains(got, `"total_tokens":8`) {
		t.Fatalf("notes / content / usage missing: %s", got)
	}
	fed := 0
	for _, rec := range r.up.all() {
		if isTranslatorCall(rec.body) {
			fed += strings.Count(extractExcerpt(rec.body), "SECRET-ORIGINAL")
		}
	}
	if fed != 1 {
		t.Fatalf("raw thinking fed to translator %d times, want exactly 1", fed)
	}
}

func TestTokenRuleCoversWholeToken(t *testing.T) {
	rules := []rule{{
		ID: "k1", Enabled: true, Scope: scopeToken, TokenID: 42, Lang: langZH,
		TranslatorGroup: "normal", TranslatorTokenID: 99, TranslatorModel: "deepseek-v4.1-flash",
		Advanced: smallAdv(),
	}}
	r := newRig(t, rules)
	const original = "SECRET-ORIGINAL-THINKING the token rule must cover a model that no group rule mentions at all"
	reply := chatStreamScript("misc-model", original)
	r.up.chatBody = func(string, string, bool) string { return reply }
	got, _ := r.post(t, "/v1/chat/completions", `{"model":"misc-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if strings.Contains(got, "SECRET-ORIGINAL") || !strings.Contains(got, "NOTE-") {
		t.Fatalf("token rule did not apply: %s", got)
	}

	cfg := r.app.cfg.get()
	if cfg.matchToken(42) == nil || cfg.matchToken(43) != nil {
		t.Fatalf("matchToken picked the wrong rule")
	}
	if cfg.match("normal", "misc-model") != nil {
		t.Fatalf("a token rule must not act as a group rule")
	}
	bad := rule{Scope: scopeToken, Lang: langZH, TranslatorTokenID: 99, TranslatorModel: "m"}
	if bad.validate() == nil {
		t.Fatalf("token rule without a token id must be rejected")
	}
	ok := rule{Scope: scopeToken, TokenID: 5, Group: "normal", Models: []string{"a"}, Lang: langZH, TranslatorTokenID: 99, TranslatorModel: "m"}
	if err := ok.validate(); err != nil || ok.Group != "" || len(ok.Models) != 0 {
		t.Fatalf("token rule should clear group/models: %v %+v", err, ok)
	}
	noKey := rule{Group: "normal", Lang: langZH, TranslatorModel: "m"}
	if noKey.validate() == nil {
		t.Fatalf("rule without a translator key must be rejected")
	}
}

// ---------- 原版思维链换回的稳健性 ----------

func TestChatDualKeysWithRoleNoLeak(t *testing.T) {
	// 一次删多个字段：保留下来的字段字节不变，删掉的一个不剩。
	body := []byte(`{"role":"assistant", "reasoning_content":"x","reasoning":"y","n":1}`)
	root, err := parseJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := deleteMembersEdit(body, root, []int{1, 2})
	if !ok || string(applyEdits(body, []jedit{e})) != `{"role":"assistant","n":1}` {
		t.Fatalf("deleteMembersEdit = %s", applyEdits(body, []jedit{e}))
	}

	r := newRig(t, withRule(smallAdv()))
	const secret = "SECRET-ORIGINAL-THINKING written into both reasoning fields of a frame that also carries the role"
	enc, _ := json.Marshal(secret)
	head := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"deepseek-v4.1-flash","choices":[`
	reply := "data: " + head + `{"index":0,"delta":{"role":"assistant","reasoning_content":` + string(enc) + `,"reasoning":` + string(enc) + `},"finish_reason":null}]}` + "\n\n" +
		"data: " + head + `{"index":0,"delta":{"content":"the answer"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	r.up.chatBody = func(string, string, bool) string { return reply }
	got, _ := r.post(t, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if strings.Contains(got, "SECRET-ORIGINAL") {
		t.Fatalf("original thinking leaked: %s", got)
	}
	if !strings.Contains(got, `"role":"assistant"`) || !strings.Contains(got, `"content":"the answer"`) {
		t.Fatalf("role / content lost: %s", got)
	}
}

func TestRestoreFallsBackToSummaryText(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	const original = "ORIGINAL-THINKING kept even when the client rewrites the system prompt every single turn"
	reply := chatStreamScript("deepseek-v4.1-flash", original)
	r.up.chatBody = func(string, string, bool) string { return reply }
	got, _ := r.post(t, "/v1/chat/completions",
		`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"now: 10:00"},{"role":"user","content":"hi"}],"stream":true}`, nil)
	shown := collectReasoning(t, got)
	if strings.TrimSpace(shown) == "" {
		t.Fatalf("no reasoning shown")
	}
	// 下一轮客户端把系统提示词里的时间改了：前缀哈希对不上，要靠摘要文字找回原版。
	next := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"now: 10:05"},{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":%s,"content":"the answer"},{"role":"user","content":"more"}]}`,
		mustJSON(t, shown))
	out, _, _, restored := prepareChat([]byte(next), 7, r.app.store)
	if restored != 1 || !bytes.Contains(out, []byte(original)) {
		t.Fatalf("original not restored after the history changed: restored=%d %s", restored, out)
	}
	// 换成另一段对话（用户说的话不同），就算摘要文字一模一样也不能换回，免得串到别的对话。
	other := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"a different chat"},{"role":"assistant","reasoning_content":%s,"content":"x"}]}`,
		mustJSON(t, shown))
	if _, _, _, n := prepareChat([]byte(other), 7, r.app.store); n != 0 {
		t.Fatalf("a different conversation must not get this original back")
	}
}

func TestFailedSummaryStillRestores(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	r.up.failNote = true
	const original = "ORIGINAL-THINKING that the translator failed to summarise, which must still go back upstream"
	r.up.chatBody = func(_ string, _ string, stream bool) string {
		if stream {
			return chatStreamScript("deepseek-v4.1-flash", original)
		}
		b, _ := json.Marshal(map[string]any{"id": "c2", "object": "chat.completion", "model": "deepseek-v4.1-flash",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "reasoning_content": original, "content": "ok"}}}})
		return string(b)
	}
	for _, stream := range []bool{true, false} {
		user := fmt.Sprintf("hi-%v", stream)
		got, _ := r.post(t, "/v1/chat/completions",
			fmt.Sprintf(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":%q}],"stream":%v}`, user, stream), nil)
		if strings.Contains(got, "ORIGINAL-THINKING") {
			t.Fatalf("stream=%v: original leaked: %s", stream, got)
		}
		// 客户端看到的思维链只有一个空格；原样带回来时要换回原版。
		next := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":%q},{"role":"assistant","reasoning_content":" ","content":"ok"}]}`, user)
		out, _, _, restored := prepareChat([]byte(next), 7, r.app.store)
		if restored != 1 || !bytes.Contains(out, []byte(original)) {
			t.Fatalf("stream=%v: original not restored after a failed summary: %s", stream, out)
		}
	}
}

func TestMessagesEmptyThinkingRestoresBySignature(t *testing.T) {
	r := newRig(t, withRule(smallAdv()))
	r.up.failNote = true
	const think = "ORIGINAL-THINKING in a Claude thinking block whose summary failed completely"
	r.up.msgBody = func(model string, _ []fakeBlock, _ bool) string {
		b, _ := json.Marshal(map[string]any{
			"id": "msg_3", "type": "message", "role": "assistant", "model": model,
			"content": []any{
				map[string]any{"type": "thinking", "thinking": think, "signature": "SIG-FAIL"},
				map[string]any{"type": "text", "text": "answer"},
			},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 3, "output_tokens": 4},
		})
		return string(b)
	}
	got, _ := r.post(t, "/v1/messages", `{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"stream":false}`, map[string]string{"x-api-key": "sk-testtoken"})
	if strings.Contains(got, "ORIGINAL-THINKING") || !strings.Contains(got, `"signature":"SIG-FAIL"`) {
		t.Fatalf("bad body: %s", got)
	}
	next := `{"model":"claude-x","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"SIG-FAIL"},{"type":"text","text":"answer"}]}]}`
	out, _, restored := prepareAnthropic([]byte(next), 7, r.app.store)
	if restored != 1 || !bytes.Contains(out, []byte(think)) || !bytes.Contains(out, []byte(`"signature":"SIG-FAIL"`)) {
		t.Fatalf("original not restored by signature: restored=%d %s", restored, out)
	}
}

func TestAcceptEncodingDroppedOnlyWhenRewriting(t *testing.T) {
	script := chatStreamScript("deepseek-v4.1-flash", "some thinking text that is long enough to be summarised by the fake translator")
	hdr := map[string]string{"Accept-Encoding": "gzip"}
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`

	r := newRig(t, withRule(smallAdv()))
	r.up.chatBody = func(string, string, bool) string { return script }
	r.post(t, "/v1/chat/completions", body, hdr)
	for _, rec := range r.up.all() {
		if !isTranslatorCall(rec.body) && rec.header.Get("Accept-Encoding") != "" {
			t.Fatalf("a rewritten request must ask for an uncompressed reply, got %q", rec.header.Get("Accept-Encoding"))
		}
	}

	plain := newRig(t, nil)
	plain.up.chatBody = func(string, string, bool) string { return script }
	plain.post(t, "/v1/chat/completions", body, hdr)
	if recs := plain.up.all(); len(recs) != 1 || recs[0].header.Get("Accept-Encoding") != "gzip" {
		t.Fatalf("pass-through must keep Accept-Encoding")
	}
}
