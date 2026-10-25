// Package thinkhide 把上游的原始思维链隐藏起来：边收边切段，每段交给转述模型
// 流式改写成简短摘要再吐给客户端；原版思维链留在服务端，客户端下一轮把摘要带回来时换回原版。
// 思路参照 Anthropic 的 summarized thinking + signature：展示的只是摘要，真正参与推理的仍是原文。
package thinkhide

import (
	"strings"
	"unicode/utf8"
)

// MinSegRunes 收尾时已有摘要的情况下，短于此值的尾巴直接丢弃；也是目标段长的下限的一半。
const MinSegRunes = 30

// Policy 控制切段的目标长度：第 k 段（从 0 数）攒到 First+Step*k 字后，在最近的段落分隔或句末切开（上限 Max）。
// 越往后每段越长，既保证第一条摘要尽快出来，又让一整块长思考只对应少数几条摘要。
// Anthropic 没有公布切段的字数，官方的口径是“一个 thinking 块对应一份摘要”，所以这里宁可粗不可碎。
type Policy struct{ First, Step, Max int }

func (p Policy) target(k int) int {
	t := p.First + p.Step*k
	if t > p.Max {
		t = p.Max
	}
	if t < 2*MinSegRunes { // 配置过小会导致切得过碎
		t = 2 * MinSegRunes
	}
	return t
}

func isSentenceEnd(buf []rune, i int) bool {
	switch buf[i] {
	case '。', '！', '？', '!', '?', '；', ';', '\n':
		return true
	case '.':
		return i+1 >= len(buf) || buf[i+1] == ' ' || buf[i+1] == '\n'
	}
	return false
}

// paraBreakEnd 如果 i 处是段落分隔（连续换行，或「）」后紧跟换行），返回分隔结束的位置（下一段的起点），否则返回 -1。
func paraBreakEnd(buf []rune, i int) int {
	if buf[i] != '\n' {
		return -1
	}
	j, nl := i, 0
	for j < len(buf) && (buf[j] == '\n' || buf[j] == '\r' || buf[j] == ' ' || buf[j] == '\t') {
		if buf[j] == '\n' {
			nl++
		}
		j++
	}
	if nl >= 2 {
		return j
	}
	if i > 0 && (buf[i-1] == '）' || buf[i-1] == ')') {
		return j
	}
	return -1
}

// TakeSegments 从思考缓冲里切出可以立刻转述的段，最多 limit 段，剩余部分原样返回。
// already 是本次回复之前已经切出的段数（决定本段的目标长度）。
//
// 一段必须先攒到目标长度，之后在目标长度之后遇到的第一个段落分隔处切开；没有段落分隔就取第一个句末；
// 目标长度的 1.5 倍内一个边界都没有，才退而求其次在逗号处切，再不行硬切。窗口还没攒满就继续等。
// 所以一整块长思考只会被切成寥寥几段，而不是每个空行一段。
func TakeSegments(buf []rune, limit, already int, pol Policy) (segs []string, rest []rune) {
	start := 0
	for len(segs) < limit {
		target := pol.target(already + len(segs))
		avail := len(buf) - start
		if avail < target {
			break
		}
		hi := target * 3 / 2
		if hi > avail {
			hi = avail
		}
		win := buf[start : start+hi]
		cut, next := -1, -1 // 本段到 win[:cut] 为止；下一段从 win[next] 起
		for i := target; i < len(win) && cut < 0; i++ {
			if e := paraBreakEnd(win, i); e >= 0 {
				cut, next = i, e
			}
		}
		for i := target; i < len(win) && cut < 0; i++ {
			if isSentenceEnd(win, i) {
				cut, next = i+1, i+1
			}
		}
		if cut < 0 {
			if hi < target*3/2 {
				break // 窗口还没攒满，再等一等边界
			}
			for i := hi - 1; i >= MinSegRunes; i-- {
				if strings.ContainsRune("，,、：: ", win[i]) {
					cut, next = i+1, i+1
					break
				}
			}
			if cut < 0 {
				cut, next = hi, hi
			}
		}
		if seg := strings.TrimSpace(string(win[:cut])); utf8.RuneCountInString(seg) > 0 {
			segs = append(segs, seg)
		}
		start += next
	}
	return segs, buf[start:]
}
