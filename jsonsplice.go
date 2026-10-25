package main

// 保留原始字节的 JSON 定位与局部改写。
//
// 需求：没有要改的字段时，请求体/响应体必须逐字节原样转发；要改时只替换命中的那几处字段，
// 其余字节（数字写法、字段顺序、转义、空白）一个都不许动。所以这里不把 JSON 解码成
// map 再序列化回去，而是解析出一棵带字节偏移的树，只对命中的区间做替换/删除。

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

type jkind byte

const (
	jObject jkind = '{'
	jArray  jkind = '['
	jString jkind = '"'
	jNumber jkind = '0'
	jBool   jkind = 'b'
	jNull   jkind = 'z'
)

// maxJSONDepth 防御性上限：正常请求远达不到，纯粹避免恶意深嵌套把栈打穿。
const maxJSONDepth = 128

type jmember struct {
	Key              string
	KeyStart, KeyEnd int
	Val              *jnode
}

type jnode struct {
	Kind       jkind
	Start, End int // 该值在原始字节里的区间 [Start, End)
	Str        string
	Bool       bool
	Num        float64
	Members    []jmember
	Items      []*jnode
}

type jparser struct {
	b     []byte
	i     int
	depth int
	err   error
}

// parseJSON 解析一个完整的 JSON 文档，失败时返回错误（调用方据此退回逐字节透传）。
func parseJSON(b []byte) (*jnode, error) {
	p := &jparser{b: b}
	p.ws()
	n := p.value()
	if p.err != nil {
		return nil, p.err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, errors.New("json: trailing data")
	}
	return n, nil
}

func (p *jparser) fail(msg string) {
	if p.err == nil {
		p.err = errors.New("json: " + msg)
	}
}

func (p *jparser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\r', '\n':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) value() *jnode {
	if p.err != nil {
		return nil
	}
	if p.i >= len(p.b) {
		p.fail("unexpected end")
		return nil
	}
	start := p.i
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, ok := p.str()
		if !ok {
			return nil
		}
		return &jnode{Kind: jString, Start: start, End: p.i, Str: s}
	case c == 't':
		if !p.literal("true") {
			return nil
		}
		return &jnode{Kind: jBool, Start: start, End: p.i, Bool: true}
	case c == 'f':
		if !p.literal("false") {
			return nil
		}
		return &jnode{Kind: jBool, Start: start, End: p.i}
	case c == 'n':
		if !p.literal("null") {
			return nil
		}
		return &jnode{Kind: jNull, Start: start, End: p.i}
	default:
		return p.number()
	}
}

func (p *jparser) literal(s string) bool {
	if !bytes.HasPrefix(p.b[p.i:], []byte(s)) {
		p.fail("bad literal")
		return false
	}
	p.i += len(s)
	return true
}

func (p *jparser) number() *jnode {
	start := p.i
	for p.i < len(p.b) {
		c := p.b[p.i]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			p.i++
			continue
		}
		break
	}
	if p.i == start {
		p.fail("bad value")
		return nil
	}
	n := &jnode{Kind: jNumber, Start: start, End: p.i}
	var f float64
	if json.Unmarshal(p.b[start:p.i], &f) == nil {
		n.Num = f
	}
	return n
}

// str 解析一个 JSON 字符串字面量（p.i 必须指向开引号），返回解码后的内容。
func (p *jparser) str() (string, bool) {
	start := p.i
	p.i++ // 开引号
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case '\\':
			p.i += 2
			continue
		case '"':
			p.i++
			raw := p.b[start:p.i]
			var s string
			if json.Unmarshal(raw, &s) != nil {
				p.fail("bad string")
				return "", false
			}
			return s, true
		default:
			p.i++
		}
	}
	p.fail("unterminated string")
	return "", false
}

func (p *jparser) object() *jnode {
	start := p.i
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxJSONDepth {
		p.fail("too deep")
		return nil
	}
	p.i++ // '{'
	n := &jnode{Kind: jObject, Start: start}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		n.End = p.i
		return n
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			p.fail("expected key")
			return nil
		}
		ks := p.i
		key, ok := p.str()
		if !ok {
			return nil
		}
		ke := p.i
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			p.fail("expected colon")
			return nil
		}
		p.i++
		p.ws()
		v := p.value()
		if p.err != nil {
			return nil
		}
		n.Members = append(n.Members, jmember{Key: key, KeyStart: ks, KeyEnd: ke, Val: v})
		p.ws()
		if p.i >= len(p.b) {
			p.fail("unexpected end in object")
			return nil
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			n.End = p.i
			return n
		default:
			p.fail("expected comma or brace")
			return nil
		}
	}
}

func (p *jparser) array() *jnode {
	start := p.i
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxJSONDepth {
		p.fail("too deep")
		return nil
	}
	p.i++ // '['
	n := &jnode{Kind: jArray, Start: start}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		n.End = p.i
		return n
	}
	for {
		p.ws()
		v := p.value()
		if p.err != nil {
			return nil
		}
		n.Items = append(n.Items, v)
		p.ws()
		if p.i >= len(p.b) {
			p.fail("unexpected end in array")
			return nil
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			n.End = p.i
			return n
		default:
			p.fail("expected comma or bracket")
			return nil
		}
	}
}

// member 返回最后一个同名成员（与 encoding/json 的键覆盖语义一致）。
func (n *jnode) member(key string) *jmember {
	if n == nil || n.Kind != jObject {
		return nil
	}
	for i := len(n.Members) - 1; i >= 0; i-- {
		if n.Members[i].Key == key {
			return &n.Members[i]
		}
	}
	return nil
}

// index 返回 key 对应的成员下标，没有时返回 -1。
func (n *jnode) index(key string) int {
	if n == nil || n.Kind != jObject {
		return -1
	}
	for i := len(n.Members) - 1; i >= 0; i-- {
		if n.Members[i].Key == key {
			return i
		}
	}
	return -1
}

func (n *jnode) str(key string) (string, bool) {
	m := n.member(key)
	if m == nil || m.Val.Kind != jString {
		return "", false
	}
	return m.Val.Str, true
}

func (n *jnode) num(key string) (float64, bool) {
	m := n.member(key)
	if m == nil || m.Val.Kind != jNumber {
		return 0, false
	}
	return m.Val.Num, true
}

// jedit 是一次字节区间替换（Repl 为空即删除）。
type jedit struct {
	Start, End int
	Repl       []byte
}

// applyEdits 按 Start 升序单趟拼接，重叠的编辑直接跳过（防御，正常不会出现）。
func applyEdits(b []byte, edits []jedit) []byte {
	if len(edits) == 0 {
		return b
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
	var out bytes.Buffer
	out.Grow(len(b))
	last := 0
	for _, e := range edits {
		if e.Start < last || e.End < e.Start || e.End > len(b) {
			continue
		}
		out.Write(b[last:e.Start])
		out.Write(e.Repl)
		last = e.End
	}
	out.Write(b[last:])
	return out.Bytes()
}

// replaceStringEdit 把某个字符串值整体换成 s（重新做 JSON 转义）。
func replaceStringEdit(n *jnode, s string) (jedit, bool) {
	if n == nil || n.Kind != jString {
		return jedit{}, false
	}
	enc, err := json.Marshal(s)
	if err != nil {
		return jedit{}, false
	}
	return jedit{Start: n.Start, End: n.End, Repl: enc}, true
}

// deleteMembersEdit 一次删掉对象里的若干成员。不能逐个删：相邻成员的删除区间会重叠，
// applyEdits 会跳过后一个，被跳过的那个字段（比如另一份思维链）就原样留下了。
// 保留下来的成员原样拷贝原始字节（键、值、转义都不动），成员之间用逗号重新连接。
func deleteMembersEdit(b []byte, obj *jnode, idxs []int) (jedit, bool) {
	if obj == nil || obj.Kind != jObject || len(idxs) == 0 {
		return jedit{}, false
	}
	drop := make(map[int]bool, len(idxs))
	for _, i := range idxs {
		if i < 0 || i >= len(obj.Members) {
			return jedit{}, false
		}
		drop[i] = true
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for i, m := range obj.Members {
		if drop[i] {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		buf.Write(b[m.KeyStart:m.Val.End])
		first = false
	}
	buf.WriteByte('}')
	return jedit{Start: obj.Start, End: obj.End, Repl: buf.Bytes()}, true
}

// substantive 判断某个成员的值是否有实质内容（去掉思维链后，delta 是否只剩空壳）。
func substantive(m *jmember) bool {
	switch m.Val.Kind {
	case jString:
		return m.Val.Str != ""
	case jNull:
		return false
	case jBool:
		return m.Val.Bool
	case jNumber:
		return true
	case jArray:
		return len(m.Val.Items) > 0
	case jObject:
		return len(m.Val.Members) > 0
	}
	return true
}

// emptyEnvelope 判断一个 chunk 去掉思维链后是否可以整条丢弃：
// 只要不带 usage / error（这两类在调用处已单独处理），其余顶层字段都是逐帧元数据
// （system_fingerprint、sla_metrics 之类），跟着空 delta 一起丢掉不会少任何信息。
func emptyEnvelope(chunk *jnode) bool {
	for i := range chunk.Members {
		switch chunk.Members[i].Key {
		case "usage", "error":
			// "usage":null 这种空值不算内容（有的上游每帧都带）。
			if substantive(&chunk.Members[i]) {
				return false
			}
		}
	}
	return true
}
