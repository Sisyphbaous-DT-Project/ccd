package thinkhide

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const leadJunk = "\"'“”‘’「」`*- \t\r\n"
const tailJunk = "\"'“”‘’「」`* \t\r\n"

// Gate 对转述模型的流式输出做“安全前缀”放行：始终扣住末尾 hold 个字不放，
// 这样任何敏感词在它的第一个字被放出去之前就能被正则匹配到。
// 命中敏感词、超长或出现换行时，在最近的句读处截断并终止本段，已放出的部分保证不含敏感词。
// 正则里单个匹配的长度不得超过 hold+1。
type Gate struct {
	blocked   *regexp.Regexp
	prefix    []rune
	hold      int
	startHold int
	maxRunes  int
	buf       []rune
	sent      int
	started   bool
	stopped   bool
	rejected  bool
	avoid     []string // 禁用的开头（不区分大小写）：转述以它们开头则一个字都不放，交给重试
}

// minKeep 命中敏感词截断后，开头就被截的残句短于此值时整段作废（交给重试），免得放出半截话。
const minKeep = 12

// NewGate 创建闸门。prefix 非空时要求转述以它开头（用于约束口吻），不符则一个字都不放。
// startHold 是第一次放字之前多扣的字数（开头就命中敏感词的整段作废）；之后每次扣 hold 个字。
func NewGate(blocked *regexp.Regexp, prefix string, hold, startHold, maxRunes int) *Gate {
	if startHold < hold {
		startHold = hold
	}
	return &Gate{blocked: blocked, prefix: []rune(prefix), hold: hold, startHold: startHold, maxRunes: maxRunes}
}

func isClauseEnd(r rune) bool {
	return strings.ContainsRune("。！？!?；;，,、\n.", r)
}

func hasRunePrefix(s, p []rune) bool {
	if len(s) < len(p) {
		return false
	}
	for i := range p {
		if s[i] != p[i] {
			return false
		}
	}
	return true
}

// SetAvoid 设置禁用的开头。只对第一次尝试设置，重试（strict）时不再强制，避免两次都被拒而丢掉这一段。
func (g *Gate) SetAvoid(prefixes []string) { g.avoid = prefixes }

// avoidState 判断 buf 与禁用开头的关系：matched 表示已经以某个禁用开头起头；
// pending 表示还太短、但仍可能成为某个禁用开头（需要再等几个字）。
func (g *Gate) avoidState() (matched, pending bool) {
	low := strings.ToLower(string(g.buf))
	for _, a := range g.avoid {
		a = strings.ToLower(a)
		if strings.HasPrefix(low, a) {
			return true, false
		}
		if len(low) < len(a) && strings.HasPrefix(a, low) {
			pending = true
		}
	}
	return false, pending
}

// Push 喂入一小段转述，返回此刻可以放行的文字，以及本段是否应当终止。
func (g *Gate) Push(piece string) (string, bool) {
	if g.stopped {
		return "", true
	}
	g.buf = append(g.buf, []rune(piece)...)
	if !g.started {
		for len(g.buf) > 0 && strings.ContainsRune(leadJunk, g.buf[0]) {
			g.buf = g.buf[1:]
		}
		if len(g.prefix) > 0 {
			if len(g.buf) < len(g.prefix) {
				return "", false
			}
			if !hasRunePrefix(g.buf, g.prefix) {
				g.stopped, g.rejected = true, true
				return "", true
			}
		}
		if len(g.buf) == 0 {
			return "", false
		}
		if len(g.avoid) > 0 {
			matched, pending := g.avoidState()
			if matched {
				g.stopped, g.rejected = true, true
				return "", true
			}
			if pending {
				return "", false
			}
		}
		g.started = true
	}
	// 转述只要一两句：出现换行就把换行之后的内容丢掉。
	truncated := false
	for i, r := range g.buf {
		if r == '\n' {
			g.buf = g.buf[:i]
			truncated = true
			break
		}
	}
	if loc := g.match(); loc >= 0 {
		return g.cutAtMin(loc), true
	}
	if g.maxRunes > 0 && len(g.buf) > g.maxRunes {
		return g.cutAt(g.maxRunes), true
	}
	if truncated {
		g.stopped = true
		return g.rest(), true
	}
	hold := g.hold
	if g.sent == 0 {
		hold = g.startHold
	}
	if safe := len(g.buf) - hold; safe > g.sent {
		out := string(g.buf[g.sent:safe])
		g.sent = safe
		return out, false
	}
	return "", false
}

// Finish 转述流结束：放出剩余的安全部分。
func (g *Gate) Finish() string {
	if g.stopped {
		return ""
	}
	if !g.started {
		if len(g.prefix) > 0 && len(g.buf) > 0 {
			g.rejected = true
		}
		g.stopped = true
		return ""
	}
	g.stopped = true
	if loc := g.match(); loc >= 0 {
		return g.cutAtMin(loc)
	}
	if g.maxRunes > 0 && len(g.buf) > g.maxRunes {
		return g.cutAt(g.maxRunes)
	}
	return g.rest()
}

// Rejected 表示因前缀不符被拒、且一个字都没放出。
func (g *Gate) Rejected() bool { return g.rejected }

func (g *Gate) match() int {
	if g.blocked == nil {
		return -1
	}
	s := string(g.buf)
	loc := g.blocked.FindStringIndex(s)
	if loc == nil {
		return -1
	}
	return utf8.RuneCountInString(s[:loc[0]])
}

// cutAt 在 limit 之前最近的句读处截断，只放出 sent 之后、句读之前的部分。
func (g *Gate) cutAt(limit int) string {
	g.stopped = true
	cut := g.sent
	for i := limit - 1; i >= g.sent; i-- {
		if isClauseEnd(g.buf[i]) {
			cut = i + 1
			break
		}
	}
	out := string(g.buf[g.sent:cut])
	g.sent = cut
	return strings.TrimRight(out, tailJunk)
}

// cutAtMin 用于敏感词截断：开头还什么都没放时，截出来的残句太短就整段作废。
func (g *Gate) cutAtMin(limit int) string {
	if g.sent == 0 {
		cut := 0
		for i := limit - 1; i >= 0; i-- {
			if isClauseEnd(g.buf[i]) {
				cut = i + 1
				break
			}
		}
		if cut < minKeep {
			g.stopped = true
			return ""
		}
	}
	return g.cutAt(limit)
}

func (g *Gate) rest() string {
	out := string(g.buf[g.sent:])
	g.sent = len(g.buf)
	return strings.TrimRight(out, tailJunk)
}
