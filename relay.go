package main

// 一次回复里的转述调度：复用 thinkhide 的切段/并行/按序输出/安全闸，本文件只提供
// 「用哪套提示词、用哪个令牌、往哪打」以及「已经写过的笔记别重复」这两件事。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Sisyphbaous-DT-Project/ccd/internal/thinkhide"
)

// recentNotes 记录本次回复里已经写完的笔记，给后面的段落当「别重复」的上下文。
// 转述是并行跑的，这里只取「当前已经完成」的那几条，不等还没跑完的。
type recentNotes struct {
	mu    sync.Mutex
	items []string
}

func (r *recentNotes) snapshot(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.items) <= n {
		return append([]string(nil), r.items...)
	}
	return append([]string(nil), r.items[len(r.items)-n:]...)
}

func (r *recentNotes) add(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, s)
	if len(r.items) > 8 {
		r.items = r.items[len(r.items)-8:]
	}
}

// relayTranslator 在 thinkhide 的 HTTP 转述官外面包一层：自己攒这次转述的成品，结束后记进 recentNotes。
type relayTranslator struct {
	inner *thinkhide.HTTPTranslator
	notes *recentNotes
}

func (t *relayTranslator) Translate(ctx context.Context, segment string, strict bool, emit func(string) bool) error {
	var sb strings.Builder
	err := t.inner.Translate(ctx, segment, strict, func(piece string) bool {
		sb.WriteString(piece)
		return emit(piece)
	})
	if sb.Len() > 0 {
		t.notes.add(sb.String())
	}
	return err
}

// newThinker 按规则创建一次回复用的转述调度器。
func (a *app) newThinker(ctx context.Context, r *rule, stream bool, notes *recentNotes) *thinkhide.Thinker {
	ps := promptFor(r.Lang, r.LangCustom)
	first, step, max := r.Advanced.policy(stream)
	hdr := http.Header{}
	if key, _, err := a.translatorKey(ctx, r.TranslatorTokenID); err == nil {
		hdr.Set("Authorization", "Bearer sk-"+key)
	} else {
		log.Printf("relay: translator key #%d unavailable (%v), this reply will have no notes", r.TranslatorTokenID, err)
	}
	inner := &thinkhide.HTTPTranslator{
		Client:      a.client,
		URL:         a.summaryURL,
		Header:      hdr,
		Model:       r.TranslatorModel,
		System:      ps.System,
		StrictNote:  ps.Strict,
		Wrap:        makeWrap(ps, notes),
		Temperature: 0.5,
		MaxTokens:   ps.MaxTokens,
		Extra:       r.Advanced.extraFields(),
	}
	cfg := &thinkhide.Config{
		Policy:        thinkhide.Policy{First: first, Step: step, Max: max},
		MaxSummaries:  0, // 条数不设上限
		Parallel:      r.Advanced.parallel(stream),
		MaxRunes:      ps.MaxRunes,
		MinTail:       ps.MinTail,
		Timeout:       r.Advanced.timeout(),
		AvoidPrefixes: ps.Avoid, // 不设任何违禁词表
		Translate:     (&relayTranslator{inner: inner, notes: notes}).Translate,
		Logf:          log.Printf,
	}
	return thinkhide.New(ctx, cfg)
}

// thinkingKeys 从代码层的名义字段名里取思维链：OpenAI 方言用 reasoning_content，
// 有的客户端/中转用 reasoning，两个都收。
var thinkingKeys = []string{"reasoning_content", "reasoning"}

// tokenFromRequest 按 new-api 的四种入口认令牌：Authorization / x-api-key / x-goog-api-key / ?key=。
// 返回原样的头值（含 Bearer 前缀），由 normalizeKey 归一化。
func tokenFromRequest(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		return v
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return "Bearer " + v
	}
	if v := r.Header.Get("x-goog-api-key"); v != "" {
		return "Bearer " + v
	}
	if v := r.URL.Query().Get("key"); v != "" {
		return "Bearer " + v
	}
	return ""
}

// compactSpan 取某个节点的紧凑 JSON 字节（去空白），用于算稳定的前缀哈希。
func compactSpan(body []byte, n *jnode) []byte {
	raw := body[n.Start:n.End]
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return raw
	}
	return buf.Bytes()
}

// prefixList 返回 messages 数组每个元素「之前」的前缀哈希，以及整个数组的前缀哈希。
// 与 Anthropic 的 signature 前缀校验同款思路：前面的对话变了，这条摘要就不再换回原版。
func prefixList(body []byte, items []*jnode) (before []string, all string) {
	h := sha256.New()
	before = make([]string, len(items))
	for i, it := range items {
		before[i] = hex.EncodeToString(h.Sum(nil))
		h.Write(compactSpan(body, it))
		h.Write([]byte{'\n'})
	}
	return before, hex.EncodeToString(h.Sum(nil))
}

// chatStoreKey 对话格式的键：前缀哈希 + 去掉空白的摘要全文。
// contentPrefixList 和 prefixList 一样逐条累加哈希，但只看对话内容本身：跳过 system / developer 消息，
// 每条只取 role、content、tool_calls、tool_call_id（思维链字段和别的元数据都不算）。
// 很多客户端每轮都会改系统提示词（比如带上当前时间），精确前缀就对不上了，这时靠它找回原版；
// 不同对话说的话不一样，不会串到别的对话。
func contentPrefixList(body []byte, items []*jnode) (before []string, all string) {
	h := sha256.New()
	before = make([]string, len(items))
	for i, it := range items {
		before[i] = "c:" + hex.EncodeToString(h.Sum(nil))
		if it.Kind != jObject {
			h.Write(compactSpan(body, it))
			h.Write([]byte{'\n'})
			continue
		}
		role, _ := it.str("role")
		if role == "system" || role == "developer" {
			continue
		}
		h.Write([]byte(role))
		for _, k := range []string{"content", "tool_calls", "tool_call_id"} {
			h.Write([]byte{0})
			if m := it.member(k); m != nil {
				h.Write(compactSpan(body, m.Val))
			}
		}
		h.Write([]byte{'\n'})
	}
	return before, "c:" + hex.EncodeToString(h.Sum(nil))
}

// chatPrefix 是一条回复「之前那段对话」的两把哈希：exact 逐字节比对，content 只看对话内容。
type chatPrefix struct{ exact, content string }

// chatPrefixes 算出 messages 每个元素之前、以及整个数组的两把前缀哈希。
func chatPrefixes(body []byte, items []*jnode) (before []chatPrefix, all chatPrefix) {
	exact, exactAll := prefixList(body, items)
	content, contentAll := contentPrefixList(body, items)
	before = make([]chatPrefix, len(items))
	for i := range items {
		before[i] = chatPrefix{exact: exact[i], content: content[i]}
	}
	return before, chatPrefix{exact: exactAll, content: contentAll}
}

func chatStoreKey(prefix, shown string) string {
	return hashKey("chat", prefix, normalizeText(shown))
}

// putChatRaw 存「摘要 → 原版」：精确前缀键、对话内容前缀键各存一份。
func putChatRaw(s *diskStore, uid int64, p chatPrefix, shown, raw string) {
	s.Put(uid, chatStoreKey(p.exact, shown), raw)
	if p.content != "" {
		s.Put(uid, chatStoreKey(p.content, shown), raw)
	}
}

// getChatRaw 先按精确前缀找，找不到再按对话内容前缀找（前面的系统提示词变了也能换回原版）。
func getChatRaw(s *diskStore, uid int64, p chatPrefix, shown string) (string, bool) {
	if raw, ok := s.Get(uid, chatStoreKey(p.exact, shown)); ok {
		return raw, true
	}
	if p.content == "" {
		return "", false
	}
	return s.Get(uid, chatStoreKey(p.content, shown))
}

// blkStoreKey 思维块的键：签名非空用签名哈希（签名与原文绑定，最可靠），否则退回摘要全文。
func blkStoreKey(signature, shown string) string {
	if signature != "" {
		return hashKey("blk", "sig", signature)
	}
	return hashKey("blk", "txt", normalizeText(shown))
}

func blkLookupKeys(signature, shown string) []string {
	keys := make([]string, 0, 2)
	if signature != "" {
		keys = append(keys, hashKey("blk", "sig", signature))
	}
	if normalizeText(shown) != "" {
		keys = append(keys, hashKey("blk", "txt", normalizeText(shown)))
	}
	return keys
}

// relayState 记录这次请求命中的规则和上下文，供响应改写阶段使用。
type relayState struct {
	kind   string // chat | anthropic
	rule   *rule
	uid    int64
	group  string
	model  string
	prefix chatPrefix
}

type relayKey struct{}

func stateFrom(ctx context.Context) *relayState {
	st, _ := ctx.Value(relayKey{}).(*relayState)
	return st
}

// contentWaiter 实现「正文最多等摘要多久」：0 表示一直等（每段本来就有超时兜底）。
type contentWaiter struct {
	limit time.Duration
	start time.Time
}

func (w *contentWaiter) begin() {
	if w.start.IsZero() {
		w.start = time.Now()
	}
}

func (w *contentWaiter) expired() bool {
	if w.limit <= 0 || w.start.IsZero() {
		return false
	}
	return time.Since(w.start) > w.limit
}
