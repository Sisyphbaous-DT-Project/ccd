package main

// Anthropic 消息格式（/v1/messages）：思维块（thinking block）逐个转述、逐个换回。
//
// 硬要求：块的数量和顺序不变；signature 一个字节都不改；signature_delta 必须等本块摘要吐完再放；
// redacted_thinking 原样不动；ping / message_delta（含 usage）原样转发。

import (
	"context"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sisyphbaous-DT-Project/ccd/internal/thinkhide"
)

// prepareAnthropic 解析 /v1/messages 请求体：取 model、把历史思维块里的摘要换回原版。
func prepareAnthropic(body []byte, uid int64, st *diskStore) (out []byte, model string, restored int) {
	root, err := parseJSON(body)
	if err != nil {
		return body, "", 0
	}
	if m, ok := root.str("model"); ok {
		model = m
	}
	msgs := root.member("messages")
	if msgs == nil || msgs.Val.Kind != jArray {
		return body, model, 0
	}
	edits := make([]jedit, 0, 8)
	for _, item := range msgs.Val.Items {
		if item.Kind != jObject {
			continue
		}
		if role, _ := item.str("role"); role != "assistant" {
			continue
		}
		content := item.member("content")
		if content == nil || content.Val.Kind != jArray {
			continue
		}
		for _, blk := range content.Val.Items {
			if blk.Kind != jObject {
				continue
			}
			if t, _ := blk.str("type"); t != "thinking" {
				continue
			}
			tm := blk.member("thinking")
			if tm == nil || tm.Val.Kind != jString {
				continue
			}
			sig, _ := blk.str("signature")
			// 摘要全失败时客户端拿到的是空文字的思维块：只要有签名，照样按签名换回原版。
			if tm.Val.Str == "" && sig == "" {
				continue
			}
			var raw string
			found := false
			for _, k := range blkLookupKeys(sig, tm.Val.Str) {
				if raw, found = st.Get(uid, k); found {
					break
				}
			}
			if !found {
				continue
			}
			if e, ok := replaceStringEdit(tm.Val, raw); ok {
				edits = append(edits, e)
				restored++
			}
		}
	}
	if len(edits) == 0 {
		return body, model, 0
	}
	return applyEdits(body, edits), model, restored
}

// anthBlock 一个正在流式到达的思维块。
type anthBlock struct {
	index     int
	th        *thinkhide.Thinker
	shown     strings.Builder
	signature string
	stored    bool
}

// selectFrameOrNote 等「上游下一帧」或「这一路摘要」或「ctx 结束」或「可选超时」。
func selectFrameOrNote(ctx context.Context, evCh <-chan sseFrame, th *thinkhide.Thinker, timer <-chan time.Time) stepResult {
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
	var ch <-chan string
	if th != nil {
		ch = th.Head()
	}
	if ch != nil {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
	}
	chosen, val, ok := reflect.Select(cases)
	switch chosen {
	case 0:
		return stepResult{kind: stepCtx}
	case 1:
		if !ok {
			return stepResult{kind: stepFrame}
		}
		f, ok2 := val.Interface().(sseFrame)
		return stepResult{kind: stepFrame, frame: f, frameOK: ok2}
	}
	if hasTimer && chosen == 2 {
		return stepResult{kind: stepTimer}
	}
	if !ok {
		return stepResult{kind: stepSummary, ev: summaryEvent{closed: true}}
	}
	piece, _ := val.Interface().(string)
	return stepResult{kind: stepSummary, ev: summaryEvent{piece: piece}}
}

// pumpAnthropic 是 /v1/messages 流式的主循环。
func (a *app) pumpAnthropic(ctx context.Context, st *relayState, body io.Reader, w io.Writer) error {
	evCh := make(chan sseFrame, 32)
	go readFrames(ctx, body, evCh)

	notes := &recentNotes{}
	waiter := &contentWaiter{limit: st.rule.Advanced.contentWait()}
	start := time.Now()
	var cur *anthBlock
	var firstSummary, firstContent time.Duration
	blocks, rawRunes, shownRunes := 0, 0, 0

	newBlock := func(index int) *anthBlock {
		return &anthBlock{index: index, th: a.newThinker(ctx, st.rule, true, notes)}
	}

	emit := func(b *anthBlock, piece string) error {
		if firstSummary == 0 {
			firstSummary = time.Since(start)
		}
		b.shown.WriteString(piece)
		return writeDataFrame(w, []byte("event: content_block_delta\n"), map[string]any{
			"type":  "content_block_delta",
			"index": b.index,
			"delta": map[string]any{"type": "thinking_delta", "thinking": piece},
		})
	}

	drain := func(b *anthBlock) error {
		if b == nil {
			return nil
		}
		b.th.Flush()
		if b.th.Pending() == 0 {
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
		for b.th.Pending() > 0 {
			res := selectFrameOrNote(ctx, nil, b.th, timer)
			switch res.kind {
			case stepCtx:
				return ctx.Err()
			case stepTimer:
				log.Printf("relay: messages content wait expired (%.0fs), dropping remaining notes", waiter.limit.Seconds())
				return nil
			case stepSummary:
				if res.ev.closed {
					b.th.Pop()
					continue
				}
				if err := emit(b, res.ev.piece); err != nil {
					return err
				}
			}
		}
		return nil
	}

	store := func(b *anthBlock) {
		if b == nil || b.stored || b.th.Raw() == "" {
			return
		}
		b.stored = true
		blocks++
		rawRunes += utf8.RuneCountInString(b.th.Raw())
		shownRunes += utf8.RuneCountInString(b.shown.String())
		shown := b.shown.String()
		if normalizeText(shown) == "" && b.signature == "" {
			return // 既没有摘要也没有签名，没法认出这个块，不存
		}
		a.store.Put(st.uid, blkStoreKey(b.signature, shown), b.th.Raw())
	}

	finish := func() {
		log.Printf("relay: messages stream model=%s group=%s rule=%s blocks=%d raw_runes=%d shown_runes=%d first_summary_ms=%d first_content_ms=%d",
			st.model, st.group, st.rule.ID, blocks, rawRunes, shownRunes,
			firstSummary.Milliseconds(), firstContent.Milliseconds())
	}

	for {
		res := selectFrameOrNote(ctx, evCh, curThinker(cur), nil)
		switch res.kind {
		case stepCtx:
			return ctx.Err()
		case stepSummary:
			if cur == nil {
				continue
			}
			if res.ev.closed {
				cur.th.Pop()
				continue
			}
			if err := emit(cur, res.ev.piece); err != nil {
				return err
			}
			continue
		case stepFrame:
			if !res.frameOK {
				_ = drain(cur)
				store(cur)
				finish()
				return nil
			}
		}

		f := res.frame
		if f.Err != nil {
			log.Printf("relay: messages upstream read error: %v", f.Err)
			_ = drain(cur)
			store(cur)
			finish()
			return nil
		}
		if !f.HasData {
			if err := writeFrame(w, f); err != nil {
				return err
			}
			continue
		}
		root, err := parseJSON(f.Data)
		if err != nil {
			if err := writeFrame(w, f); err != nil {
				return err
			}
			continue
		}
		typ, _ := root.str("type")
		switch typ {
		case "content_block_start":
			idx := 0
			if v, ok := root.num("index"); ok {
				idx = int(v)
			}
			cb := root.member("content_block")
			btype := ""
			if cb != nil && cb.Val.Kind == jObject {
				btype, _ = cb.Val.str("type")
			}
			if btype == "thinking" {
				cur = newBlock(idx)
				if sig, ok := cb.Val.str("signature"); ok {
					cur.signature = sig
				}
				// 少数实现会在 start 帧里就带第一段思考：收下来并把文字清空，不能外发。
				if tm := cb.Val.member("thinking"); tm != nil && tm.Val.Kind == jString && tm.Val.Str != "" {
					cur.th.Add(tm.Val.Str)
					if e, ok := replaceStringEdit(tm.Val, ""); ok {
						if err := writeDataRaw(w, f.Event, applyEdits(f.Data, []jedit{e})); err != nil {
							return err
						}
						continue
					}
				}
				if err := writeFrame(w, f); err != nil {
					return err
				}
				continue
			}
			if err := writeFrame(w, f); err != nil {
				return err
			}

		case "content_block_delta":
			d := root.member("delta")
			dtype := ""
			if d != nil && d.Val.Kind == jObject {
				dtype, _ = d.Val.str("type")
			}
			switch dtype {
			case "thinking_delta":
				if cur == nil {
					continue
				}
				if t, ok := d.Val.str("thinking"); ok && t != "" {
					cur.th.Add(t)
				}
				continue // 原版思维链永远不转发
			case "signature_delta":
				if sig, ok := d.Val.str("signature"); ok {
					if cur != nil {
						cur.signature = sig
					}
				}
				if err := drain(cur); err != nil {
					return err
				}
				if err := writeFrame(w, f); err != nil {
					return err
				}
				continue
			}
			if cur != nil {
				if err := drain(cur); err != nil {
					return err
				}
			}
			if err := writeFrame(w, f); err != nil {
				return err
			}

		case "content_block_stop":
			if cur != nil {
				if err := drain(cur); err != nil {
					return err
				}
				store(cur)
				cur = nil
			}
			if err := writeFrame(w, f); err != nil {
				return err
			}

		case "message_delta", "message_stop":
			if cur != nil {
				if err := drain(cur); err != nil {
					return err
				}
				store(cur)
				if typ == "message_stop" {
					cur = nil
				}
			}
			if firstContent == 0 {
				firstContent = time.Since(start)
			}
			if err := writeFrame(w, f); err != nil {
				return err
			}
			if typ == "message_stop" {
				finish()
			}

		case "error":
			// 上游报错原样转发；排队的摘要没必要再等（正文都已经不来了）。
			cur = nil
			if err := writeFrame(w, f); err != nil {
				return err
			}

		default:
			if err := writeFrame(w, f); err != nil {
				return err
			}
		}
	}
}

func curThinker(b *anthBlock) *thinkhide.Thinker {
	if b == nil {
		return nil
	}
	return b.th
}

// anthropicStream 把上游 SSE 改写后从管道交给 ReverseProxy。
func (a *app) anthropicStream(resp *http.Response, st *relayState) io.ReadCloser {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(resp.Request.Context())
	upstreamBody := resp.Body // 同 chat.go：换成管道读端之前先存下上游 body，否则协程会读到自己
	go func() {
		defer cancel()
		defer upstreamBody.Close()
		pw.CloseWithError(a.pumpAnthropic(ctx, st, upstreamBody, pw))
	}()
	return &pipeBody{PipeReader: pr, cancel: cancel}
}

// anthropicNonStream 改写同步响应：每个 thinking 块单独转述，签名原样保留。
func (a *app) anthropicNonStream(resp *http.Response, st *relayState) error {
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
	blocks, segs, rawRunes, shownRunes := 0, 0, 0, 0
	content := root.member("content")
	if content != nil && content.Val.Kind == jArray {
		for _, blk := range content.Val.Items {
			if blk.Kind != jObject {
				continue
			}
			if t, _ := blk.str("type"); t != "thinking" {
				continue
			}
			tm := blk.member("thinking")
			if tm == nil || tm.Val.Kind != jString {
				continue
			}
			sig, _ := blk.str("signature")
			raw := tm.Val.Str
			if strings.TrimSpace(raw) == "" {
				continue
			}
			shown, n := a.summarizeAll(resp.Request.Context(), st.rule, raw)
			blocks++
			segs += n
			rawRunes += utf8.RuneCountInString(raw)
			shownRunes += utf8.RuneCountInString(shown)
			if e, ok := replaceStringEdit(tm.Val, shown); ok {
				edits = append(edits, e)
			}
			if strings.TrimSpace(shown) != "" || sig != "" {
				a.store.Put(st.uid, blkStoreKey(sig, shown), raw)
			}
		}
	}
	if len(edits) > 0 {
		body = applyEdits(body, edits)
	}
	setBody(resp, body)
	log.Printf("relay: messages sync model=%s group=%s rule=%s blocks=%d segs=%d raw_runes=%d shown_runes=%d",
		st.model, st.group, st.rule.ID, blocks, segs, rawRunes, shownRunes)
	return nil
}
