package main

// OpenAI 对话格式（/v1/chat/completions）：请求侧换回原版、响应侧把思维链换成转述。
//
// 原则：没命中的字节一个都不动。流式按帧处理，不需要改写的帧整段原样写出；
// 改写只发生在「这一帧里带了 reasoning_content / reasoning」的时候。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sisyphbaous-DT-Project/ccd/internal/thinkhide"
)

func isThinkingKey(k string) bool {
	for _, t := range thinkingKeys {
		if t == k {
			return true
		}
	}
	return false
}

// nonNullMember 这个键存在且值不是 null。
func nonNullMember(n *jnode, key string) bool {
	m := n.member(key)
	return m != nil && m.Val.Kind != jNull
}

// choiceDroppable 判断某个 choice 去掉思维链后是否可以整条丢弃。
func choiceDroppable(cn *jnode) bool {
	if cn == nil || cn.Kind != jObject {
		return false
	}
	for i := range cn.Members {
		m := &cn.Members[i]
		switch m.Key {
		case "index", "delta", "finish_reason":
		case "logprobs":
			if substantive(m) {
				return false
			}
		default:
			if substantive(m) {
				return false
			}
		}
	}
	delta := cn.member("delta")
	if delta == nil || delta.Val.Kind != jObject {
		return false
	}
	for i := range delta.Val.Members {
		if isThinkingKey(delta.Val.Members[i].Key) {
			continue
		}
		if substantive(&delta.Val.Members[i]) {
			return false
		}
	}
	return true
}

// prepareChat 解析对话请求体：取 model、算出前缀哈希、把历史里能换回的摘要换回原版。
// 解析失败或没有要换回的内容时返回原始字节，保证逐字节透传。
func prepareChat(body []byte, uid int64, st *diskStore) (out []byte, model string, prefix chatPrefix, restored int) {
	root, err := parseJSON(body)
	if err != nil {
		return body, "", chatPrefix{}, 0
	}
	if m, ok := root.str("model"); ok {
		model = m
	}
	msgs := root.member("messages")
	if msgs == nil || msgs.Val.Kind != jArray {
		return body, model, chatPrefix{}, 0
	}
	before, all := chatPrefixes(body, msgs.Val.Items)
	edits := make([]jedit, 0, 8)
	for i, item := range msgs.Val.Items {
		if item.Kind != jObject {
			continue
		}
		if role, _ := item.str("role"); role != "assistant" {
			continue
		}
		for _, k := range thinkingKeys {
			m := item.member(k)
			if m == nil || m.Val.Kind != jString || m.Val.Str == "" {
				continue
			}
			raw, ok := getChatRaw(st, uid, before[i], m.Val.Str)
			if !ok {
				continue
			}
			if e, ok := replaceStringEdit(m.Val, raw); ok {
				edits = append(edits, e)
				restored++
			}
		}
	}
	if len(edits) == 0 {
		return body, model, all, 0
	}
	return applyEdits(body, edits), model, all, restored
}

// chatEnvelope 记住上游第一帧的外壳字段，用来拼摘要帧（id / created / model 必须与上游一致）。
type chatEnvelope struct {
	id          json.RawMessage
	object      json.RawMessage
	created     json.RawMessage
	model       json.RawMessage
	fingerprint json.RawMessage
	set         bool
}

func (e *chatEnvelope) capture(root *jnode, body []byte) {
	if e.set {
		return
	}
	pick := func(key string) json.RawMessage {
		if m := root.member(key); m != nil {
			return body[m.Val.Start:m.Val.End]
		}
		return nil
	}
	e.id, e.object, e.created, e.model = pick("id"), pick("object"), pick("created"), pick("model")
	e.fingerprint = pick("system_fingerprint")
	e.set = true
}

func (e *chatEnvelope) summaryChunk(index int, piece string) []byte {
	enc, _ := json.Marshal(piece)
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	writeOrNull(&b, e.id)
	b.WriteString(`,"object":`)
	if len(e.object) > 0 {
		b.Write(e.object)
	} else {
		b.WriteString(`"chat.completion.chunk"`)
	}
	b.WriteString(`,"created":`)
	writeOrNull(&b, e.created)
	b.WriteString(`,"model":`)
	writeOrNull(&b, e.model)
	if len(e.fingerprint) > 0 {
		b.WriteString(`,"system_fingerprint":`)
		b.Write(e.fingerprint)
	}
	b.WriteString(`,"choices":[{"index":`)
	b.WriteString(strconv.Itoa(index))
	b.WriteString(`,"delta":{"reasoning_content":`)
	b.Write(enc)
	b.WriteString(`},"finish_reason":null}]}`)
	return b.Bytes()
}

func writeOrNull(b *bytes.Buffer, raw json.RawMessage) {
	if len(raw) > 0 {
		b.Write(raw)
		return
	}
	b.WriteString("null")
}

// chatChoice 是单个 choice 的状态：自己的转述调度器和已经吐出的摘要。
type chatChoice struct {
	index int
	th    *thinkhide.Thinker
	shown strings.Builder
}

type summaryEvent struct {
	choice *chatChoice
	piece  string
	closed bool
}

const (
	stepCtx = iota
	stepFrame
	stepSummary
	stepTimer
)

type stepResult struct {
	kind    int
	frame   sseFrame
	frameOK bool
	ev      summaryEvent
}

// neverFrame 是一个永远不会就绪的通道，用来占住 reflect.Select 里「上游帧」这一格。
var neverFrame = make(chan sseFrame)

// selectStep 同时等「上游下一帧」「某一路摘要」「ctx 结束」「可选超时」。
// 用 reflect.Select 是因为 choice 数量不固定（n>1 时会有多路）。
func selectStep(ctx context.Context, evCh <-chan sseFrame, choices []*chatChoice, timer <-chan time.Time) stepResult {
	if evCh == nil {
		evCh = neverFrame
	}
	cases := []reflect.SelectCase{
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(evCh)},
	}
	hasTimer := timer != nil
	if hasTimer {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timer)})
	}
	base := len(cases)
	owners := make([]int, 0, len(choices))
	for i, c := range choices {
		ch := c.th.Head()
		if ch == nil {
			continue
		}
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
		owners = append(owners, i)
	}
	chosen, val, ok := reflect.Select(cases)
	switch chosen {
	case 0:
		return stepResult{kind: stepCtx}
	case 1:
		f, ok2 := val.Interface().(sseFrame)
		return stepResult{kind: stepFrame, frame: f, frameOK: ok2}
	}
	if hasTimer && chosen == 2 {
		return stepResult{kind: stepTimer}
	}
	c := choices[owners[chosen-base]]
	if !ok {
		return stepResult{kind: stepSummary, ev: summaryEvent{choice: c, closed: true}}
	}
	piece, _ := val.Interface().(string)
	return stepResult{kind: stepSummary, ev: summaryEvent{choice: c, piece: piece}}
}

// pumpChat 是对话流式的主循环：上游每来一帧，把思维链交给转述调度器，
// 其余原样写出；摘要按顺序一小截一小截插进去；正文、工具调用、结束帧到来前先把排队的摘要吐完。
func (a *app) pumpChat(ctx context.Context, st *relayState, body io.Reader, w io.Writer) error {
	evCh := make(chan sseFrame, 32)
	go readFrames(ctx, body, evCh)
	cw := &countingWriter{w: w}

	notes := &recentNotes{}
	env := &chatEnvelope{}
	var choices []*chatChoice
	waiter := &contentWaiter{limit: st.rule.Advanced.contentWait()}
	start := time.Now()
	var firstSummary, firstContent time.Duration
	pinged := false

	getChoice := func(idx int) *chatChoice {
		for _, c := range choices {
			if c.index == idx {
				return c
			}
		}
		c := &chatChoice{index: idx, th: a.newThinker(ctx, st.rule, true, notes)}
		choices = append(choices, c)
		return c
	}

	pending := func() int {
		n := 0
		for _, c := range choices {
			n += c.th.Pending()
		}
		return n
	}

	emit := func(c *chatChoice, piece string) error {
		if firstSummary == 0 {
			firstSummary = time.Since(start)
		}
		c.shown.WriteString(piece)
		return writeDataRaw(cw, nil, env.summaryChunk(c.index, piece))
	}

	flush := func() error {
		for _, c := range choices {
			c.th.Flush()
		}
		if pending() == 0 {
			return nil
		}
		var timer <-chan time.Time
		if waiter.limit > 0 {
			waiter.begin()
			left := waiter.limit - time.Since(waiter.start)
			if left <= 0 {
				left = time.Millisecond
			}
			tt := time.NewTimer(left)
			defer tt.Stop()
			timer = tt.C
		}
		for pending() > 0 {
			res := selectStep(ctx, nil, choices, timer)
			switch res.kind {
			case stepCtx:
				return ctx.Err()
			case stepTimer:
				log.Printf("relay: chat content wait expired (%.0fs), dropping remaining notes", waiter.limit.Seconds())
				return nil
			case stepSummary:
				if res.ev.closed {
					res.ev.choice.th.Pop()
					continue
				}
				if err := emit(res.ev.choice, res.ev.piece); err != nil {
					return err
				}
			}
		}
		return nil
	}

	finish := func() {
		for _, c := range choices {
			if c.th.Raw() == "" {
				continue
			}
			// 摘要全失败时客户端只收到首包心跳的那个空格：也照样存（只认前缀键），下一轮带回来时照样换回原版。
			putChatRaw(a.store, st.uid, st.prefix, c.shown.String(), c.th.Raw())
		}
		log.Printf("relay: chat stream model=%s group=%s rule=%s segs=%d raw_runes=%d shown_runes=%d first_summary_ms=%d first_content_ms=%d",
			st.model, st.group, st.rule.ID, totalPushes(choices), totalRunes(choices),
			totalShown(choices), firstSummary.Milliseconds(), firstContent.Milliseconds())
	}

	for {
		res := selectStep(ctx, evCh, choices, nil)
		switch res.kind {
		case stepCtx:
			return ctx.Err()
		case stepSummary:
			if res.ev.closed {
				res.ev.choice.th.Pop()
				continue
			}
			if err := emit(res.ev.choice, res.ev.piece); err != nil {
				return err
			}
			continue
		case stepFrame:
			if !res.frameOK {
				_ = flush()
				finish()
				return nil
			}
		}

		f := res.frame
		if f.Err != nil {
			log.Printf("relay: chat upstream read error: %v", f.Err)
			_ = flush()
			finish()
			return nil
		}
		if !f.HasData {
			if err := writeFrame(cw, f); err != nil {
				return err
			}
			continue
		}
		if string(f.Data) == "[DONE]" {
			if err := flush(); err != nil {
				return err
			}
			finish()
			return writeFrame(cw, f)
		}
		root, err := parseJSON(f.Data)
		if err != nil {
			if err := writeFrame(cw, f); err != nil {
				return err
			}
			continue
		}
		env.capture(root, f.Data)
		// 用量帧、上游错误帧：先把排队的摘要吐完再放；这一帧里要是也夹着思维链，照样删掉，原版不能外发。
		// 有的上游每帧都带 "usage":null，那种不算，不然每帧都得停下来等摘要。
		tail := nonNullMember(root, "usage") || nonNullMember(root, "error")

		doFlush, removed, droppable := tail, false, !tail
		pingIdx := 0
		var edits []jedit
		chNode := root.member("choices")
		if chNode == nil || chNode.Val.Kind != jArray || len(chNode.Val.Items) == 0 {
			droppable = false
		} else {
			for _, cn := range chNode.Val.Items {
				if cn.Kind != jObject {
					droppable = false
					continue
				}
				idx := 0
				if v, ok := cn.num("index"); ok {
					idx = int(v)
				}
				c := getChoice(idx)
				if fr := cn.member("finish_reason"); fr != nil && fr.Val.Kind != jNull {
					doFlush, droppable = true, false
				}
				delta := cn.member("delta")
				if delta == nil || delta.Val.Kind != jObject {
					droppable = false
					continue
				}
				added := false
				var drop []int
				for _, k := range thinkingKeys {
					i := delta.Val.index(k)
					if i < 0 {
						continue
					}
					m := &delta.Val.Members[i]
					// 有的上游为兼容把同一段思考同时写进 reasoning_content 和 reasoning：只收一份，免得原文记重。
					if m.Val.Kind == jString && m.Val.Str != "" && !added {
						c.th.Add(m.Val.Str)
						added = true
					}
					drop = append(drop, i)
					removed = true
					pingIdx = idx
				}
				// 同一个 delta 里的几个思维链字段必须一次删掉，逐个删会漏掉一个。
				if e, ok := deleteMembersEdit(f.Data, delta.Val, drop); ok {
					edits = append(edits, e)
				}
				if s, ok := delta.Val.str("content"); ok && s != "" {
					doFlush = true
				}
				if m := delta.Val.member("tool_calls"); m != nil && substantive(m) {
					doFlush = true
				}
				if !choiceDroppable(cn) {
					droppable = false
				}
			}
			if !emptyEnvelope(root) {
				droppable = false
			}
		}

		if doFlush {
			if err := flush(); err != nil {
				return err
			}
			if firstContent == 0 && !tail {
				firstContent = time.Since(start)
			}
		}
		if removed && droppable {
			// 首包心跳：这一帧去掉思维链后只剩空壳，本来要整条丢掉；但如果它还是本次回复的
			// 第一条数据帧，就先放一条只含一个空格的 reasoning 帧（不含任何思维链文字），
			// 让客户端尽早收到首包、显示「思考中」，不用干等第一条摘要。
			// （ccd 在 new-api 前面，new-api 记的首字时间不受这里影响。）
			if !pinged {
				pinged = true
				if err := writeDataRaw(cw, nil, env.summaryChunk(pingIdx, " ")); err != nil {
					return err
				}
			}
			continue
		}
		if removed {
			if err := writeDataRaw(cw, f.Event, applyEdits(f.Data, edits)); err != nil {
				return err
			}
			continue
		}
		if err := writeFrame(cw, f); err != nil {
			return err
		}
	}
}

func totalPushes(cs []*chatChoice) int {
	n := 0
	for _, c := range cs {
		n += c.th.Pushes()
	}
	return n
}

func totalRunes(cs []*chatChoice) int {
	n := 0
	for _, c := range cs {
		n += utf8.RuneCountInString(c.th.Raw())
	}
	return n
}

func totalShown(cs []*chatChoice) int {
	n := 0
	for _, c := range cs {
		n += utf8.RuneCountInString(c.shown.String())
	}
	return n
}

// chatStream 把上游 SSE 改写后从管道交给 ReverseProxy。
func (a *app) chatStream(resp *http.Response, st *relayState) io.ReadCloser {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(resp.Request.Context())
	upstreamBody := resp.Body // 换成管道读端之前先存下上游 body，否则协程会读到自己
	go func() {
		defer cancel()
		defer upstreamBody.Close()
		pw.CloseWithError(a.pumpChat(ctx, st, upstreamBody, pw))
	}()
	return &pipeBody{PipeReader: pr, cancel: cancel}
}

// summarizeAll 同步回复用：整段思维链按段并行转述，按顺序拼成摘要。
func (a *app) summarizeAll(ctx context.Context, r *rule, raw string) (string, int) {
	notes := &recentNotes{}
	th := a.newThinker(ctx, r, false, notes)
	th.Add(raw)
	th.Flush()
	var sb strings.Builder
	_ = th.Drain(func(p string) error { sb.WriteString(p); return nil })
	return sb.String(), th.Pushes()
}

// chatNonStream 改写同步响应：思维链整段拿到后并行转述，替换 reasoning_content。
func (a *app) chatNonStream(resp *http.Response, st *relayState) error {
	body, err := readBody(resp)
	if err != nil {
		return err
	}
	root, err := parseJSON(body)
	if err != nil {
		setBody(resp, body)
		return nil
	}
	edits := make([]jedit, 0, 4)
	segs, rawRunes, shownRunes := 0, 0, 0
	chNode := root.member("choices")
	if chNode != nil && chNode.Val.Kind == jArray {
		for _, cn := range chNode.Val.Items {
			msg := cn.member("message")
			if msg == nil || msg.Val.Kind != jObject {
				continue
			}
			lastRaw, lastShown := "", ""
			for _, k := range thinkingKeys {
				m := msg.Val.member(k)
				if m == nil || m.Val.Kind != jString {
					continue
				}
				if strings.TrimSpace(m.Val.Str) == "" {
					continue // 原文本来就只有空白，没有可藏的内容，原样保留
				}
				raw := m.Val.Str
				shown := lastShown
				if raw != lastRaw {
					// 两个键写的是同一段思考时只转述一次，两处换成同一份摘要。
					var n int
					shown, n = a.summarizeAll(resp.Request.Context(), st.rule, raw)
					segs += n
					rawRunes += utf8.RuneCountInString(raw)
					shownRunes += utf8.RuneCountInString(shown)
					lastRaw, lastShown = raw, shown
				}
				if strings.TrimSpace(shown) == "" {
					// 转述全失败：留一个空格占位（不含任何思维链文字），原版照样存下，下一轮带回来时还能换回。
					shown = " "
				}
				if e, ok := replaceStringEdit(m.Val, shown); ok {
					edits = append(edits, e)
					putChatRaw(a.store, st.uid, st.prefix, shown, raw)
				}
			}
		}
	}
	if len(edits) > 0 {
		body = applyEdits(body, edits)
	}
	setBody(resp, body)
	log.Printf("relay: chat sync model=%s group=%s rule=%s segs=%d raw_runes=%d shown_runes=%d",
		st.model, st.group, st.rule.ID, segs, rawRunes, shownRunes)
	return nil
}
