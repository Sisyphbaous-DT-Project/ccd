package thinkhide

import (
	"context"
	"log"
	"math"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Translator 把一段原始思考流式改写成摘要：每产生一小截文字就调用 emit；emit 返回 false 表示
// 消费方不再需要（被闸门截断或 ctx 已取消），应当尽快停止并返回 nil。strict 为 true 表示上一次被拒后的重试。
type Translator func(ctx context.Context, segment string, strict bool, emit func(piece string) bool) error

// Config 一次回复里隐藏思维链所需的全部配置。
type Config struct {
	Policy        Policy
	MaxSummaries  int           // 每条回复最多转述几段，<=0 表示不设上限
	Parallel      int           // 同时进行的转述数，<=0 用 3
	Hold          int           // 闸门扣留字数，<=0 用 20
	MinTail       int           // 收尾时已有摘要的情况下，短于此字数的尾巴直接丢弃，<=0 用 MinSegRunes
	StartHold     int           // 第一次放字前多扣的字数，<=0 用 36
	MaxRunes      int           // 单段转述最长字数，<=0 用 240
	Timeout       time.Duration // 单次转述超时，<=0 用 20 秒
	Blocked       *regexp.Regexp
	Prefix        string
	AvoidPrefixes []string // 禁用的开头：第一次尝试以它们开头则整段作废并以 strict 重试，不会放出一个字
	Translate     Translator
	Logf          func(format string, args ...any)
}

func (c *Config) maxSummaries() int {
	if c.MaxSummaries > 0 {
		return c.MaxSummaries
	}
	return math.MaxInt32 // 不设上限
}

func (c *Config) parallel() int {
	if c.Parallel > 0 {
		return c.Parallel
	}
	return 3
}

func (c *Config) hold() int {
	if c.Hold > 0 {
		return c.Hold
	}
	return 20
}

func (c *Config) minTail() int {
	if c.MinTail > 0 {
		return c.MinTail
	}
	return MinSegRunes
}

func (c *Config) startHold() int {
	if c.StartHold > 0 {
		return c.StartHold
	}
	return 36
}

func (c *Config) maxRunes() int {
	if c.MaxRunes > 0 {
		return c.MaxRunes
	}
	return 240
}

func (c *Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 20 * time.Second
}

func (c *Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Thinker 在一次回复里拦截原始思维链：原文只留在内存里，边收边切段，每段交给转述模型流式改写，
// 转述结果按段落顺序排队，由主循环一小截一小截写给客户端。原文永远不外发。
//
// 失败时没有任何兜底话术：转述出错或被闸门拒绝时重试一次（strict），仍不行就略过这一段。
type Thinker struct {
	cfg        *Config
	ctx        context.Context
	rbuf       []rune
	raw        strings.Builder
	pushes     int
	pending    []chan string
	sem        chan struct{}
	failStreak atomic.Int32
	seed       uint32 // 每次回复一个随机种子，供转述挑开头句式的起点
}

// New 创建 Thinker。
func New(ctx context.Context, cfg *Config) *Thinker {
	return &Thinker{cfg: cfg, ctx: ctx, sem: make(chan struct{}, cfg.parallel()), seed: uint32(time.Now().UnixNano() >> 9)}
}

// Add 收下一小截原始思考，并把已经够一段的部分立刻提交转述（最后一个名额留给收尾）。
func (t *Thinker) Add(rc string) {
	t.raw.WriteString(rc)
	t.rbuf = append(t.rbuf, []rune(rc)...)
	budget := t.cfg.maxSummaries() - 1 - t.pushes
	if budget <= 0 {
		return
	}
	segs, rest := TakeSegments(t.rbuf, budget, t.pushes, t.cfg.Policy)
	t.rbuf = append([]rune(nil), rest...)
	for _, s := range segs {
		t.push(s)
	}
}

// Flush 思考结束（正文、工具调用或结束标记到来）：把还没转述的尾巴转述掉。
// 已经有摘要且尾巴很短时直接丢弃，省得正文白等一次转述。
func (t *Thinker) Flush() {
	text := strings.TrimSpace(string(t.rbuf))
	t.rbuf = nil
	if text == "" || t.pushes >= t.cfg.maxSummaries() {
		return
	}
	n := utf8.RuneCountInString(text)
	if (t.pushes > 0 && n < t.cfg.minTail()) || n < 8 {
		return
	}
	t.push(text)
}

// Head 返回队首转述的输出通道（转述结束时关闭）；没有排队的转述时返回 nil，select 里永远不会选中。
func (t *Thinker) Head() <-chan string {
	if len(t.pending) == 0 {
		return nil
	}
	return t.pending[0]
}

// Pop 丢掉已经读完的队首。
func (t *Thinker) Pop() { t.pending = t.pending[1:] }

// Pending 排队中的转述数。
func (t *Thinker) Pending() int { return len(t.pending) }

// Pushes 本次回复累计提交的转述数。
func (t *Thinker) Pushes() int { return t.pushes }

// Raw 本次回复收到的完整原始思考。
func (t *Thinker) Raw() string { return t.raw.String() }

// Drain 按顺序把所有已提交的转述吐完；emit 返回错误则终止。
func (t *Thinker) Drain(emit func(piece string) error) error {
	for len(t.pending) > 0 {
		select {
		case piece, ok := <-t.pending[0]:
			if !ok {
				t.Pop()
				continue
			}
			if err := emit(piece); err != nil {
				return err
			}
		case <-t.ctx.Done():
			return t.ctx.Err()
		}
	}
	return nil
}

func (t *Thinker) push(text string) {
	ch := make(chan string, 256)
	idx := t.pushes
	t.pushes++
	t.pending = append(t.pending, ch)
	go t.run(text, ch, idx)
}

type ctxKey int

const (
	keyIdx ctxKey = iota
	keySeed
)

// WithSegment 把段序号和本次回复的种子放进 ctx（Thinker 内部调用；测试里也可以直接用）。
func WithSegment(ctx context.Context, idx int, seed uint32) context.Context {
	return context.WithValue(context.WithValue(ctx, keyIdx, idx), keySeed, seed)
}

// SegmentIndex 转述官据此知道这是本次回复的第几段（从 0 数），用来按序号轮换开头句式。
func SegmentIndex(ctx context.Context) int {
	v, _ := ctx.Value(keyIdx).(int)
	return v
}

// ReplySeed 每次回复一个随机种子：不同回复的开头句式从不同位置起算。
func ReplySeed(ctx context.Context) uint32 {
	v, _ := ctx.Value(keySeed).(uint32)
	return v
}

func (t *Thinker) run(text string, ch chan string, idx int) {
	defer close(ch)
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-t.ctx.Done():
		return
	}
	if t.failStreak.Load() >= 2 {
		t.cfg.logf("thinkhide: skip segment, translator unhealthy")
		return
	}
	sent := false
	var lastErr error
	for attempt := 0; attempt < 2 && !sent && t.ctx.Err() == nil; attempt++ {
		g := NewGate(t.cfg.Blocked, t.cfg.Prefix, t.cfg.hold(), t.cfg.startHold(), t.cfg.maxRunes())
		if attempt == 0 {
			g.SetAvoid(t.cfg.AvoidPrefixes)
		}
		alive := true
		send := func(s string) {
			if s == "" || !alive {
				return
			}
			select {
			case ch <- s:
				sent = true
			case <-t.ctx.Done():
				alive = false
			}
		}
		actx, cancel := context.WithTimeout(t.ctx, t.cfg.timeout())
		actx = WithSegment(actx, idx, t.seed)
		err := t.cfg.Translate(actx, text, attempt == 1, func(piece string) bool {
			out, stop := g.Push(piece)
			send(out)
			return alive && !stop
		})
		cancel()
		lastErr = err
		if err == nil || sent {
			send(g.Finish())
		}
	}
	switch {
	case sent:
		t.failStreak.Store(0)
		select {
		case ch <- "\n\n":
		case <-t.ctx.Done():
		}
	case lastErr != nil && t.ctx.Err() == nil:
		t.failStreak.Add(1)
		t.cfg.logf("thinkhide: segment dropped, translator error: %v", lastErr)
	case t.ctx.Err() == nil:
		t.cfg.logf("thinkhide: segment dropped, translator output rejected")
	}
}
