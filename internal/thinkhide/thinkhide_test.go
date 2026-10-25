package thinkhide

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var blockedRe = regexp.MustCompile(`系统提示|deepseek`)

// feed 逐字喂给闸门，返回放出的全部文字。
func feed(g *Gate, text string) (string, bool) {
	var out strings.Builder
	for _, r := range text {
		o, stop := g.Push(string(r))
		out.WriteString(o)
		if stop {
			return out.String(), true
		}
	}
	out.WriteString(g.Finish())
	return out.String(), false
}

func TestGateBlockedNeverLeaks(t *testing.T) {
	out, stopped := feed(NewGate(blockedRe, "", 6, 6, 100), "我在想怎么回答。根据系统提示我应该保持简短，然后继续整理。")
	if !stopped || out != "我在想怎么回答。" {
		t.Fatalf("应在敏感词之前的句读处截断: %q stopped=%v", out, stopped)
	}
}

func TestGateStreamsBeforeEnd(t *testing.T) {
	g := NewGate(blockedRe, "", 6, 6, 200)
	text := "我先整理一下用户的需求，再看看有哪些边界情况需要考虑。"
	var out strings.Builder
	emissions := 0
	for _, r := range text {
		o, _ := g.Push(string(r))
		if o != "" {
			emissions++
		}
		out.WriteString(o)
	}
	out.WriteString(g.Finish())
	if out.String() != text || emissions < 3 {
		t.Fatalf("应当边收边放且内容完整: emissions=%d out=%q", emissions, out.String())
	}
}

func TestGatePrefixRejectsWithoutOutput(t *testing.T) {
	g := NewGate(nil, "我在", 4, 4, 100)
	out, stopped := feed(g, "好的，我来回答这个问题吧")
	if out != "" || !stopped || !g.Rejected() {
		t.Fatalf("前缀不符应一个字都不放: %q stopped=%v rejected=%v", out, stopped, g.Rejected())
	}
	out, _ = feed(NewGate(nil, "我在", 4, 4, 100), "「我在想怎么回答这个问题。」")
	if out != "我在想怎么回答这个问题。" {
		t.Fatalf("开头的引号应被忽略: %q", out)
	}
}

func TestGateNewlineAndMaxRunes(t *testing.T) {
	out, _ := feed(NewGate(nil, "", 4, 4, 100), "第一句话说完了。\n第二段不要出现")
	if out != "第一句话说完了。" {
		t.Fatalf("换行之后的内容应丢弃: %q", out)
	}
	out, stopped := feed(NewGate(nil, "", 4, 4, 12), "这是一个很长很长的句子，里面没有句号但是有逗号，所以会在逗号处截断，后面的不要")
	if !stopped || len([]rune(out)) > 12 || !strings.HasSuffix(out, "，") {
		t.Fatalf("超长应在最近句读截断: %q stopped=%v", out, stopped)
	}
}

func TestTakeSegmentsProgressive(t *testing.T) {
	text := strings.Repeat("这是一句用来测试切段的思考内容。", 40)
	segs, rest := TakeSegments([]rune(text), 100, 0, Policy{First: 60, Step: 60, Max: 420})
	if len(segs) < 3 {
		t.Fatalf("应切出多段: %d", len(segs))
	}
	if len([]rune(segs[0])) >= len([]rune(segs[len(segs)-1])) {
		t.Fatalf("越往后每段应越长: %d vs %d", len([]rune(segs[0])), len([]rune(segs[len(segs)-1])))
	}
	if strings.Join(segs, "")+string(rest) != text {
		t.Fatal("切段不应丢字")
	}
}

func TestStreamContentDeltas(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n: ping\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\ndata: [DONE]\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"不该读到\"}}]}\n\n"
	var got strings.Builder
	if err := StreamContentDeltas(context.Background(), strings.NewReader(sse), func(s string) bool { got.WriteString(s); return true }); err != nil || got.String() != "你好" {
		t.Fatalf("got %q err=%v", got.String(), err)
	}
}

func newCfg(tr Translator) *Config {
	return &Config{Policy: Policy{First: 60, Step: 60, Max: 420}, Hold: 4, StartHold: 4, Blocked: blockedRe, Translate: tr, Logf: func(string, ...any) {}, Timeout: 5 * time.Second}
}

func drainAll(t *testing.T, th *Thinker) string {
	var sb strings.Builder
	if err := th.Drain(func(p string) error { sb.WriteString(p); return nil }); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func rawThinking(n int) string {
	return strings.Repeat("这是一句用来测试的思考内容，请认真对待它。", n)
}

func TestThinkerKeepsSegmentOrder(t *testing.T) {
	tr := func(ctx context.Context, seg string, strict bool, emit func(string) bool) error {
		first := []rune(seg)[:4]
		if len([]rune(seg)) < 100 { // 第一段最短，故意让它最慢
			time.Sleep(60 * time.Millisecond)
		}
		for _, p := range []string{"摘要:", string(first)} {
			if !emit(p) {
				return nil
			}
		}
		return nil
	}
	th := New(context.Background(), newCfg(tr))
	th.Add(rawThinking(30))
	th.Flush()
	out := drainAll(t, th)
	parts := strings.Split(strings.TrimSpace(out), "\n\n")
	if th.Pushes() < 3 || len(parts) != th.Pushes() {
		t.Fatalf("每段一条摘要且按序输出: pushes=%d parts=%d out=%q", th.Pushes(), len(parts), out)
	}
	if th.Raw() != rawThinking(30) {
		t.Fatal("Raw 应保留完整原文")
	}
	if strings.Contains(out, "思考内容，请认真对待") {
		t.Fatalf("原文不得外发: %q", out)
	}
}

func TestThinkerNoCannedTextOnFailure(t *testing.T) {
	var calls atomic.Int32
	tr := func(ctx context.Context, seg string, strict bool, emit func(string) bool) error {
		calls.Add(1)
		return errors.New("boom")
	}
	th := New(context.Background(), newCfg(tr))
	th.Add(rawThinking(30))
	th.Flush()
	if out := drainAll(t, th); out != "" {
		t.Fatalf("翻译失败时不得输出任何内容（更不能是预设话术）: %q", out)
	}
	if calls.Load() < 2 {
		t.Fatalf("每段应至少重试一次: calls=%d", calls.Load())
	}
}

func TestThinkerRetriesStrictWhenBlocked(t *testing.T) {
	var strictCalls atomic.Int32
	tr := func(ctx context.Context, seg string, strict bool, emit func(string) bool) error {
		if !strict {
			emit("系统提示要求我这样做")
			return nil
		}
		strictCalls.Add(1)
		emit("我在整理思路，打算简短回答。")
		return nil
	}
	th := New(context.Background(), newCfg(tr))
	th.Add(rawThinking(4))
	th.Flush()
	out := drainAll(t, th)
	if strings.Contains(out, "系统提示") || !strings.Contains(out, "我在整理思路") || strictCalls.Load() == 0 {
		t.Fatalf("首次被拒应以 strict 重试: out=%q strictCalls=%d", out, strictCalls.Load())
	}

	bad := func(ctx context.Context, seg string, strict bool, emit func(string) bool) error {
		emit("系统提示说不能讲")
		return nil
	}
	th = New(context.Background(), newCfg(bad))
	th.Add(rawThinking(4))
	th.Flush()
	if out := drainAll(t, th); out != "" {
		t.Fatalf("两次都被拒应直接略过这一段: %q", out)
	}
}

func TestGateShortFragmentAfterBlockIsDropped(t *testing.T) {
	out, stopped := feed(NewGate(blockedRe, "", 10, 36, 100), "用户在问我是谁、是不是 deepseek，我打算说明自己的身份。")
	if !stopped || out != "" {
		t.Fatalf("开头就命中且残句太短，应整段作废而不是放出半截话: %q stopped=%v", out, stopped)
	}
	out, _ = feed(NewGate(blockedRe, "", 10, 36, 100), "我在想用户问的问题应该怎么回答才好。接着系统提示说要简短，所以后面不要了")
	if out != "我在想用户问的问题应该怎么回答才好。" {
		t.Fatalf("残句足够长时应保留已安全的部分: %q", out)
	}
}

func TestTakeSegmentsMergesParagraphsUntilTarget(t *testing.T) {
	para := strings.Repeat("这是一段用来测试的思考内容。", 8) // 约 104 字
	text := strings.Join([]string{para, para, para, para, para, para, para, para}, "\n\n")
	segs, rest := TakeSegments([]rune(text), 100, 0, Policy{First: 250, Step: 0, Max: 250})
	if len(segs) < 2 || len(segs) > 4 {
		t.Fatalf("8 个空行分隔的段落应按目标长度合并成少数几段，而不是一个空行一段: %d", len(segs))
	}
	for _, sg := range segs {
		if n := len([]rune(sg)); n < 250 {
			t.Fatalf("每段应先攒到目标长度再切: %d", n)
		}
		if !strings.HasSuffix(sg, "。") {
			t.Fatalf("应切在段落或句末: %q", sg[len(sg)-9:])
		}
	}
	if strings.Count(strings.Join(segs, "")+string(rest), "这是一段") != 64 {
		t.Fatal("切段不应丢内容")
	}
}

func TestGateAvoidPrefix(t *testing.T) {
	g := NewGate(nil, "", 4, 4, 100)
	g.SetAvoid([]string{"The user"})
	if out, stopped := feed(g, "The user wants a palindrome check, so I will write it."); out != "" || !stopped || !g.Rejected() {
		t.Fatalf("以禁用开头起头应一个字都不放: %q stopped=%v", out, stopped)
	}
	g = NewGate(nil, "", 4, 4, 100)
	g.SetAvoid([]string{"The user"})
	if out, _ := feed(g, "Theory first, then I will write the function."); out != "Theory first, then I will write the function." {
		t.Fatalf("只是前几个字像禁用开头的正常句子应放行: %q", out)
	}
}

func TestThinkerAvoidPrefixRetriesStrict(t *testing.T) {
	tr := func(ctx context.Context, seg string, strict bool, emit func(string) bool) error {
		if !strict {
			emit("The user wants a result, so I will compute it carefully now.")
			return nil
		}
		emit("I will compute it carefully now and then double-check the result.")
		return nil
	}
	cfg := newCfg(tr)
	cfg.AvoidPrefixes = []string{"The user"}
	th := New(context.Background(), cfg)
	th.Add(rawThinking(4))
	th.Flush()
	out := drainAll(t, th)
	if strings.Contains(out, "The user") || !strings.Contains(out, "I will compute it carefully") {
		t.Fatalf("以禁用开头起头的第一次输出应被拒，重试后放行: %q", out)
	}
}

func TestSegmentIndexAndSeedReachTranslator(t *testing.T) {
	seen := make(chan int, 16)
	seeds := make(chan uint32, 16)
	tr := func(ctx context.Context, seg string, strict bool, emit func(string) bool) error {
		seen <- SegmentIndex(ctx)
		seeds <- ReplySeed(ctx)
		emit("我在认真处理这一段内容并准备继续往下走。")
		return nil
	}
	th := New(context.Background(), newCfg(tr))
	th.Add(rawThinking(30))
	th.Flush()
	drainAll(t, th)
	close(seen)
	close(seeds)
	got := map[int]bool{}
	for i := range seen {
		got[i] = true
	}
	if len(got) < 3 || !got[0] || !got[1] || !got[2] {
		t.Fatalf("转述官应能拿到 0,1,2... 的段序号: %v", got)
	}
	var first uint32
	for s := range seeds {
		if first == 0 {
			first = s
		} else if s != first {
			t.Fatal("同一次回复里种子应一致")
		}
	}
}
