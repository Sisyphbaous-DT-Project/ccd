package main

// Responses 协议（/v1/responses）：思维链是独立的 reasoning 条目，文本在 response.output[].summary[].text。
//
// 流式时逐字来自 response.reasoning_summary_text.delta；完整文本还会在
// reasoning_summary_text.done、reasoning_summary_part.done、output_item.done、
// response.completed 四处重复出现，必须一起换成同一份摘要，否则客户端拼出来的仍是原版。
// 正文、工具调用、usage、id、sequence_number、事件顺序一律不动；没有可改的内容时逐字节透传。

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sisyphbaous-DT-Project/ccd/internal/thinkhide"
)

// respDeltaEvent 是没有 event: 行可用时的兜底（正常都用上游原帧的 event 行）。
const respDeltaEvent = "event: response.reasoning_summary_text.delta\n"

// respItem 是一个正在流式到达的 reasoning 条目。
type respItem struct {
	id    string
	index int
	th    *thinkhide.Thinker
	shown strings.Builder
	tpl   []byte // 最近一帧 delta 的原始 data：借用它的 item_id / output_index / sequence_number 拼摘要帧
	tplEv []byte
}

// respItemAt 按 item_id（优先）或 output_index 找这次回复里的 reasoning 条目。
// output_item.done / completed 里的条目对象没有 item_id，用它们自己的 id。
func respItemAt(root *jnode) (id string, index int) {
	index = -1
	if v, ok := root.str("item_id"); ok && v != "" {
		id = v
	} else if v, ok := root.str("id"); ok {
		id = v
	}
	if v, ok := root.num("output_index"); ok {
		index = int(v)
	}
	return id, index
}

// prepareResponses 解析 /v1/responses 请求体：取 model、算前缀哈希、把历史 reasoning 条目里的摘要换回原版。
func prepareResponses(body []byte, uid int64, st *diskStore) (out []byte, model string, prefix chatPrefix, restored int) {
	root, err := parseJSON(body)
	if err != nil {
		return body, "", chatPrefix{}, 0
	}
	if m, ok := root.str("model"); ok {
		model = m
	}
	in := root.member("input")
	if in == nil || in.Val.Kind != jArray {
		return body, model, chatPrefix{}, 0
	}
	before, all := chatPrefixes(body, in.Val.Items)
	edits := make([]jedit, 0, 8)
	for i, item := range in.Val.Items {
		if item.Kind != jObject {
			continue
		}
		if t, _ := item.str("type"); t != "reasoning" {
			continue
		}
		restored += restoreRespParts(item, before[i], uid, st, &edits)
	}
	if len(edits) == 0 {
		return body, model, all, 0
	}
	return applyEdits(body, edits), model, all, restored
}

// restoreRespParts 把 summary / content 里能认出来的摘要换回原版（两处写法都可能出现）。
func restoreRespParts(item *jnode, prefix chatPrefix, uid int64, st *diskStore, edits *[]jedit) int {
	n := 0
	for _, field := range []string{"summary", "content"} {
		arr := item.member(field)
		if arr == nil || arr.Val.Kind != jArray {
			continue
		}
		for _, part := range arr.Val.Items {
			if part.Kind != jObject {
				continue
			}
			tm := part.member("text")
			if tm == nil || tm.Val.Kind != jString || tm.Val.Str == "" {
				continue
			}
			if prefix.exact == "" {
				continue
			}
			raw, ok := st.Get(uid, chatStoreKey(prefix.exact, tm.Val.Str))
			if !ok {
				continue
			}
			if e, ok := replaceStringEdit(tm.Val, raw); ok {
				*edits = append(*edits, e)
				n++
			}
		}
	}
	return n
}

// reasoningTextEdits 把某个 reasoning 条目里 summary / content 的文本全换成 shown。
func reasoningTextEdits(n *jnode, shown string) []jedit {
	if n == nil || n.Kind != jObject {
		return nil
	}
	if t, ok := n.str("type"); !ok || t != "reasoning" {
		return nil
	}
	var edits []jedit
	for _, field := range []string{"summary", "content"} {
		arr := n.member(field)
		if arr == nil || arr.Val.Kind != jArray {
			continue
		}
		for _, part := range arr.Val.Items {
			if part.Kind != jObject {
				continue
			}
			tm := part.member("text")
			if tm == nil || tm.Val.Kind != jString {
				continue
			}
			if e, ok := replaceStringEdit(tm.Val, shown); ok {
				edits = append(edits, e)
			}
		}
	}
	return edits
}

// walkReasoning 深度遍历一帧 JSON，对每个 type=reasoning 的对象回调。
func walkReasoning(n *jnode, fn func(*jnode)) {
	if n == nil {
		return
	}
	switch n.Kind {
	case jObject:
		if t, ok := n.str("type"); ok && t == "reasoning" {
			fn(n)
		}
		for i := range n.Members {
			walkReasoning(n.Members[i].Val, fn)
		}
	case jArray:
		for _, it := range n.Items {
			walkReasoning(it, fn)
		}
	}
}

// findRespItem 按 id / output_index 认条目；认不出来时退回第一条（宁可换错也不漏原版）。
func findRespItem(items []*respItem, n *jnode) *respItem {
	id, index := respItemAt(n)
	for _, it := range items {
		if id != "" && it.id == id {
			return it
		}
	}
	if index >= 0 {
		for _, it := range items {
			if it.index == index {
				return it
			}
		}
	}
	if len(items) == 1 {
		return items[0]
	}
	return nil
}

// shownText 摘要全失败时给一个空格占位（不含任何原版文字），与 chat 的做法一致。
func shownText(it *respItem) string {
	s := it.shown.String()
	if strings.TrimSpace(s) == "" {
		return " "
	}
	return s
}

// pumpResponses 是 /v1/responses 流式的主循环。
//
// 摘要的等待交给 thinkhide 的 Drain：每收一截原始思考就先把已经跑完的摘要吐出去，
// 遇到 done / 正文 / 结束帧再把尾巴转述干净。这样顺序天然是「思考 → 正文」，
// 不需要再自己维护一套多路 select。
func (a *app) pumpResponses(ctx context.Context, st *relayState, body io.Reader, w io.Writer) error {
	evCh := make(chan sseFrame, 32)
	go readFrames(ctx, body, evCh)

	notes := &recentNotes{}
	start := time.Now()
	var firstSummary, firstContent time.Duration
	var items []*respItem
	finished := false

	getItem := func(id string, index int) *respItem {
		for _, it := range items {
			if id != "" && it.id == id {
				return it
			}
			if id == "" && index >= 0 && it.index == index {
				return it
			}
		}
		if id == "" && index < 0 {
			return nil
		}
		it := &respItem{id: id, index: index, th: a.newThinker(ctx, st.rule, true, notes)}
		items = append(items, it)
		return it
	}

	emit := func(it *respItem, piece string) error {
		if firstSummary == 0 {
			firstSummary = time.Since(start)
		}
		it.shown.WriteString(piece)
		if len(it.tpl) == 0 {
			return nil // 还没收到过 delta 帧，摘要留给后面的 done / completed 帧带出去
		}
		root, err := parseJSON(it.tpl)
		if err != nil {
			return nil
		}
		m := root.member("delta")
		if m == nil || m.Val.Kind != jString {
			return nil
		}
		e, ok := replaceStringEdit(m.Val, piece)
		if !ok {
			return nil
		}
		ev := it.tplEv
		if len(ev) == 0 {
			ev = []byte(respDeltaEvent)
		}
		return writeDataRaw(w, ev, applyEdits(it.tpl, []jedit{e}))
	}

	// drain 按顺序吐出所有已提交的摘要；flush 为真时先把没转述的尾巴也提交。
	drain := func(flush bool) error {
		for _, it := range items {
			if flush {
				it.th.Flush()
			}
			if err := it.th.Drain(func(piece string) error { return emit(it, piece) }); err != nil {
				return err
			}
		}
		return nil
	}

	finish := func() {
		if finished {
			return
		}
		finished = true
		// 只按精确前缀存一份：内容前缀兜底对 responses 的 function_call /
		// function_call_output 不敏感，会把不同对话认成同一条；少存一份也省一半文件。
		// input 不是数组时（字符串形式）没有可用前缀，不存，避免所有请求挤同一个键。
		if st.prefix.exact != "" {
			for _, it := range items {
				raw := it.th.Raw()
				if strings.TrimSpace(raw) == "" {
					continue
				}
				a.store.Put(st.uid, chatStoreKey(st.prefix.exact, shownText(it)), raw)
			}
		}
		log.Printf("relay: responses stream model=%s group=%s rule=%s segs=%d raw_runes=%d shown_runes=%d first_summary_ms=%d first_content_ms=%d",
			st.model, st.group, st.rule.ID, totalRespPushes(items), totalRespRaw(items),
			totalRespShown(items), firstSummary.Milliseconds(), firstContent.Milliseconds())
	}

	// patchFrame 把一帧里所有 reasoning 条目的完整文本换成摘要。
	patchFrame := func(f sseFrame) ([]byte, bool) {
		root, err := parseJSON(f.Data)
		if err != nil {
			return nil, false
		}
		var edits []jedit
		walkReasoning(root, func(n *jnode) {
			if it := findRespItem(items, n); it != nil {
				edits = append(edits, reasoningTextEdits(n, shownText(it))...)
			}
		})
		if len(edits) == 0 {
			return nil, false
		}
		return applyEdits(f.Data, edits), true
	}

	writeOne := func(f sseFrame) error {
		if f.Err != nil {
			log.Printf("relay: responses upstream read error: %v", f.Err)
			_ = drain(true)
			finish()
			return nil
		}
		if !f.HasData {
			return writeFrame(w, f)
		}
		root, err := parseJSON(f.Data)
		if err != nil {
			return writeFrame(w, f)
		}
		typ, _ := root.str("type")
		switch typ {
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			id, index := respItemAt(root)
			it := getItem(id, index)
			if it == nil {
				return writeFrame(w, f)
			}
			it.tpl = append([]byte(nil), f.Data...)
			it.tplEv = append([]byte(nil), f.Event...)
			if s, ok := root.str("delta"); ok && s != "" {
				it.th.Add(s)
			}
			// 这一段要是已经攒够，先把跑完的摘要吐出去（没攒够时这里是空转）。
			return drain(false)

		case "response.reasoning_summary_text.done", "response.reasoning_text.done":
			id, index := respItemAt(root)
			it := getItem(id, index)
			if it == nil {
				return writeFrame(w, f)
			}
			// 有的上游只发完整文本、不发逐字增量：先把原文喂给调度器，否则原版会丢。
			if it.th.Raw() == "" {
				if s, ok := root.str("text"); ok && s != "" {
					it.th.Add(s)
				}
			}
			if err := drain(true); err != nil {
				return err
			}
			tm := root.member("text")
			if tm != nil && tm.Val.Kind == jString {
				if e, ok := replaceStringEdit(tm.Val, shownText(it)); ok {
					return writeDataRaw(w, f.Event, applyEdits(f.Data, []jedit{e}))
				}
			}
			return writeFrame(w, f)

		case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
			// 上游在 done 帧里会带上完整原文；added 帧现在是空的，万一回填也一起换掉。
			if err := drain(true); err != nil {
				return err
			}
			id, index := respItemAt(root)
			if it := getItem(id, index); it != nil {
				if part := root.member("part"); part != nil && part.Val.Kind == jObject {
					if tm := part.Val.member("text"); tm != nil && tm.Val.Kind == jString && tm.Val.Str != "" {
						if e, ok := replaceStringEdit(tm.Val, shownText(it)); ok {
							return writeDataRaw(w, f.Event, applyEdits(f.Data, []jedit{e}))
						}
					}
				}
			}
			return writeFrame(w, f)

		case "response.output_item.done":
			if err := drain(true); err != nil {
				return err
			}
			if body, ok := patchFrame(f); ok {
				return writeDataRaw(w, f.Event, body)
			}
			return writeFrame(w, f)

		case "response.completed", "response.incomplete", "response.failed":
			// 这三种收尾帧都带完整 response 对象，output 里可能有整段原文：
			// 只处理 completed 会让被截断（incomplete）和中途失败（failed）的回复把原版漏出去。
			if err := drain(true); err != nil {
				return err
			}
			finish()
			if body, ok := patchFrame(f); ok {
				return writeDataRaw(w, f.Event, body)
			}
			return writeFrame(w, f)

		case "error":
			if err := drain(true); err != nil {
				return err
			}
			finish()
			return writeFrame(w, f)

		case "response.output_item.added":
			// 现在上游这一帧是空摘要；万一哪天回填原文，这里也过一遍改写，别等着漏。
			if err := drain(true); err != nil {
				return err
			}
			if body, ok := patchFrame(f); ok {
				return writeDataRaw(w, f.Event, body)
			}
			return writeFrame(w, f)

		default:
			// 正文、工具调用、usage、各种启停事件：排队的摘要先吐完，
			// 保证客户端看到的顺序还是「思考 → 正文」。
			if err := drain(true); err != nil {
				return err
			}
			switch typ {
			case "response.created", "response.in_progress", "response.output_item.added",
				"response.reasoning_summary_part.added", "response.content_part.added":
			default:
				if firstContent == 0 {
					firstContent = time.Since(start)
				}
			}
			return writeFrame(w, f)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-evCh:
			if !ok {
				_ = drain(true)
				finish()
				return nil
			}
			if err := writeOne(f); err != nil {
				return err
			}
		}
	}
}

func totalRespPushes(items []*respItem) int {
	n := 0
	for _, it := range items {
		n += it.th.Pushes()
	}
	return n
}

func totalRespRaw(items []*respItem) int {
	n := 0
	for _, it := range items {
		n += utf8.RuneCountInString(it.th.Raw())
	}
	return n
}

func totalRespShown(items []*respItem) int {
	n := 0
	for _, it := range items {
		n += utf8.RuneCountInString(it.shown.String())
	}
	return n
}

// responsesStream 把上游 SSE 改写后从管道交给 ReverseProxy。
func (a *app) responsesStream(resp *http.Response, st *relayState) io.ReadCloser {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(resp.Request.Context())
	upstreamBody := resp.Body
	go func() {
		defer cancel()
		defer upstreamBody.Close()
		pw.CloseWithError(a.pumpResponses(ctx, st, upstreamBody, pw))
	}()
	return &pipeBody{PipeReader: pr, cancel: cancel}
}

// responsesNonStream 改写同步响应：每个 reasoning 条目单独转述。
func (a *app) responsesNonStream(resp *http.Response, st *relayState) error {
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
	walkReasoning(root, func(n *jnode) {
		var raw strings.Builder
		for _, field := range []string{"summary", "content"} {
			arr := n.member(field)
			if arr == nil || arr.Val.Kind != jArray {
				continue
			}
			for _, part := range arr.Val.Items {
				if part.Kind != jObject {
					continue
				}
				if tm := part.member("text"); tm != nil && tm.Val.Kind == jString {
					raw.WriteString(tm.Val.Str)
				}
			}
		}
		text := raw.String()
		if strings.TrimSpace(text) == "" {
			return
		}
		shown, n2 := a.summarizeAll(resp.Request.Context(), st.rule, text)
		blocks++
		segs += n2
		rawRunes += utf8.RuneCountInString(text)
		shownRunes += utf8.RuneCountInString(shown)
		if strings.TrimSpace(shown) == "" {
			shown = " "
		}
		edits = append(edits, reasoningTextEdits(n, shown)...)
		putChatRaw(a.store, st.uid, st.prefix, shown, text)
	})
	if len(edits) > 0 {
		body = applyEdits(body, edits)
	}
	setBody(resp, body)
	log.Printf("relay: responses sync model=%s group=%s rule=%s blocks=%d segs=%d raw_runes=%d shown_runes=%d",
		st.model, st.group, st.rule.ID, blocks, segs, rawRunes, shownRunes)
	return nil
}
