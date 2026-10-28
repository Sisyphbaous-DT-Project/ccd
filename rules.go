package main

// 规则配置：哪个分组 / 哪些模型（或某一个令牌）要转述、用哪个模型转述、转述成什么语言，以及高级参数。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	cfgVersion = 1
	maxRules   = 200

	scopeToken = "token" // 规则作用于某一个令牌：这个令牌发的请求全部套上转述
)

// advanced 是每条规则的高级设置，全部留空即用默认值。
type advanced struct {
	StreamFirst    int             `json:"stream_first,omitempty"`
	StreamStep     int             `json:"stream_step,omitempty"`
	StreamMax      int             `json:"stream_max,omitempty"`
	SyncFirst      int             `json:"sync_first,omitempty"`
	SyncStep       int             `json:"sync_step,omitempty"`
	SyncMax        int             `json:"sync_max,omitempty"`
	Parallel       int             `json:"parallel,omitempty"`      // 每个请求的流式转述并发
	SyncParallel   int             `json:"sync_parallel,omitempty"` // 每个请求的同步转述并发
	TimeoutSec     int             `json:"timeout_sec,omitempty"`   // 单段转述超时
	ContentWaitSec int             `json:"content_wait_sec,omitempty"`
	Extra          json.RawMessage `json:"extra,omitempty"` // 关思考参数，只用在转述调用上
}

type rule struct {
	ID              string   `json:"id"`
	Enabled         bool     `json:"enabled"`
	Scope           string   `json:"scope,omitempty"` // 空 = 按分组；token = 只管某一个令牌
	TokenID         int64    `json:"token_id,omitempty"`
	TokenName       string   `json:"token_name,omitempty"` // 只给面板显示用
	Group           string   `json:"group"`
	Models          []string `json:"models"` // 空 = 整个分组
	Lang            string   `json:"lang"`   // zh | en | custom
	LangCustom      string   `json:"lang_custom,omitempty"`
	TranslatorGroup string   `json:"translator_group"`
	TranslatorModel string   `json:"translator_model"`
	// 转述用的 key（管理员自己名下已有的令牌），转述分组以这个 key 实际所在的分组为准
	TranslatorTokenID   int64     `json:"translator_token_id,omitempty"`
	TranslatorTokenName string    `json:"translator_token_name,omitempty"` // 只给面板显示用
	Advanced            *advanced `json:"advanced,omitempty"`
}

type config struct {
	Version int    `json:"version"`
	Rules   []rule `json:"rules"`
}

func defaultConfig() *config {
	return &config{Version: cfgVersion}
}

// clamp 把越界的数字拉回安全范围：配置由前端传上来，不能盲信。
func (a *advanced) clamp() {
	pos := func(v *int, def, min, max int) {
		if *v <= 0 {
			*v = def
		}
		if *v < min {
			*v = min
		}
		if *v > max {
			*v = max
		}
	}
	pos(&a.StreamFirst, 100, 40, 4000)
	pos(&a.StreamStep, 200, 0, 4000)
	pos(&a.StreamMax, 1800, 200, 20000)
	pos(&a.SyncFirst, 250, 40, 4000)
	pos(&a.SyncStep, 150, 0, 4000)
	pos(&a.SyncMax, 1800, 200, 20000)
	pos(&a.Parallel, 3, 1, 16)
	pos(&a.SyncParallel, 6, 1, 32)
	pos(&a.TimeoutSec, 20, 3, 300)
	if a.ContentWaitSec < 0 {
		a.ContentWaitSec = 0
	}
	if a.ContentWaitSec > 600 {
		a.ContentWaitSec = 600
	}
}

func (a *advanced) policy(stream bool) (first, step, max int) {
	if a == nil {
		if stream {
			return 100, 200, 1800
		}
		return 250, 150, 1800
	}
	if stream {
		return a.StreamFirst, a.StreamStep, a.StreamMax
	}
	return a.SyncFirst, a.SyncStep, a.SyncMax
}

func (a *advanced) parallel(stream bool) int {
	if a == nil {
		if stream {
			return 3
		}
		return 6
	}
	if stream {
		return a.Parallel
	}
	return a.SyncParallel
}

func (a *advanced) timeout() time.Duration {
	if a == nil || a.TimeoutSec <= 0 {
		return 20 * time.Second
	}
	return time.Duration(a.TimeoutSec) * time.Second
}

func (a *advanced) contentWait() time.Duration {
	if a == nil || a.ContentWaitSec <= 0 {
		return 0
	}
	return time.Duration(a.ContentWaitSec) * time.Second
}

// normalizeExtra 把「关思考参数」规整成紧凑的 JSON 对象字节。
//
// 面板里的输入框是一段文本，提交上来时 extra 是 JSON 字符串（"{\"a\":1}"），
// 直接调接口的人给的是对象本身；两种都收，统一存成对象字节。
// 以前只认对象，面板填对了也报「必须是 JSON 对象」，整条规则连带存不进去。
func (a *advanced) normalizeExtra() error {
	bad := errors.New("高级设置里的关思考参数必须是 JSON 对象")
	raw := bytes.TrimSpace(a.Extra)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		a.Extra = nil
		return nil
	}
	if raw[0] == '"' { // 面板发来的字符串：先解成文本，再按 JSON 解析一次
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return bad
		}
		raw = bytes.TrimSpace([]byte(s))
		if len(raw) == 0 {
			a.Extra = nil
			return nil
		}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return bad
	}
	if len(m) == 0 {
		a.Extra = nil
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return bad
	}
	a.Extra = b
	return nil
}

// extraFields 解析「关思考参数」。默认同时给 reasoning_effort=off 和 thinking.type=disabled：
// 不同上游只认其中一种写法，只认后者的上游如果只收到 off，会先偷偷思考吃掉 max_tokens，摘要写到一半就被截断。
func (a *advanced) extraFields() map[string]any {
	def := map[string]any{"reasoning_effort": "off", "thinking": map[string]any{"type": "disabled"}}
	if a == nil || len(a.Extra) == 0 {
		return def
	}
	var m map[string]any
	if json.Unmarshal(a.Extra, &m) != nil || len(m) == 0 {
		return def
	}
	return m
}

// validateTranslator 只校验「转述」相关的字段（语言 + 转述模型），
// 管理界面的「测试」按钮没有分组，走这一层。
func (r *rule) validateTranslator() error {
	r.TranslatorGroup = strings.TrimSpace(r.TranslatorGroup)
	r.TranslatorModel = strings.TrimSpace(r.TranslatorModel)
	r.LangCustom = strings.TrimSpace(r.LangCustom)
	r.TranslatorTokenName = strings.TrimSpace(r.TranslatorTokenName)
	if r.TranslatorTokenID <= 0 {
		return errors.New("请选择转述用的 key")
	}
	if r.TranslatorModel == "" {
		return errors.New("请选择转述用的模型")
	}
	if r.TranslatorGroup == "auto" {
		return errors.New("转述用的 key 不能是 auto 分组的，请换一个固定分组的 key")
	}
	switch r.Lang {
	case langEN, langZH:
		r.LangCustom = ""
	case langCustom:
		if r.LangCustom == "" {
			return errors.New("自定义语言不能为空")
		}
		if len([]rune(r.LangCustom)) > 40 {
			return errors.New("自定义语言最多 40 个字")
		}
	default:
		return errors.New("转述语言只能是中文、English 或自定义")
	}
	if r.Advanced != nil {
		r.Advanced.clamp()
		if err := r.Advanced.normalizeExtra(); err != nil {
			return err
		}
	}
	return nil
}

// validate 校验并规整一条规则；错误信息会原样回给前端。
func (r *rule) validate() error {
	if r.Scope == scopeToken {
		if r.TokenID <= 0 {
			return errors.New("请选择要套用转述的令牌")
		}
		r.Group, r.Models = "", []string{}
		r.TokenName = strings.TrimSpace(r.TokenName)
		return r.validateTranslator()
	}
	r.Scope, r.TokenID, r.TokenName = "", 0, ""
	r.Group = strings.TrimSpace(r.Group)
	if r.Group == "" {
		return errors.New("请选择要转述的分组")
	}
	if err := r.validateTranslator(); err != nil {
		return err
	}
	seen := map[string]bool{}
	models := make([]string, 0, len(r.Models))
	for _, m := range r.Models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		models = append(models, m)
	}
	r.Models = models
	return nil
}

func (c *config) normalize() {
	if c.Version == 0 {
		c.Version = cfgVersion
	}
	if len(c.Rules) > maxRules {
		c.Rules = c.Rules[:maxRules]
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.ID == "" {
			r.ID = fmt.Sprintf("r%d", i+1)
		}
		if r.Models == nil {
			r.Models = []string{}
		}
		if r.Lang == "" {
			r.Lang = langZH
		}
		if err := r.validate(); err != nil {
			// 坏数据只影响这一条：关掉它，别让整份配置起不来。
			r.Enabled = false
		}
		// 停用中的规则存半成品时校验会提前返回，关思考参数可能还是面板发来的字符串；
		// 这里统一收成对象，免得落盘的格式随「有没有填完」变来变去。
		if r.Advanced != nil {
			if err := r.Advanced.normalizeExtra(); err != nil {
				r.Advanced.Extra = nil // 真填坏了就退回默认，别把坏值留在盘上
			}
		}
	}
}

// match 找命中的规则：指定模型的规则优先于整组规则；同级按配置顺序取第一条。
func (c *config) match(group, model string) *rule {
	if c == nil || group == "" {
		return nil
	}
	var whole *rule
	for i := range c.Rules {
		r := &c.Rules[i]
		if !r.Enabled || r.Scope == scopeToken || r.Group != group {
			continue
		}
		if len(r.Models) == 0 {
			if whole == nil {
				whole = r
			}
			continue
		}
		for _, m := range r.Models {
			if m == model {
				return r
			}
		}
	}
	return whole
}

// groupHasRules 该分组有没有任何启用中的规则（auto 分组会传入多个候选）。
func (c *config) groupHasRules(group string) bool {
	if c == nil {
		return false
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Enabled && r.Scope != scopeToken && r.Group == group {
			return true
		}
	}
	return false
}

// matchToken 找作用于这个令牌的规则（同一令牌有多条时按顺序取第一条启用的），比分组规则优先。
func (c *config) matchToken(tokenID int64) *rule {
	if c == nil || tokenID <= 0 {
		return nil
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Enabled && r.Scope == scopeToken && r.TokenID == tokenID {
			return r
		}
	}
	return nil
}

func (c *config) tokenHasRules(tokenID int64) bool { return c.matchToken(tokenID) != nil }

func (c *config) clone() *config {
	b, err := json.Marshal(c)
	if err != nil {
		return defaultConfig()
	}
	var out config
	if json.Unmarshal(b, &out) != nil {
		return defaultConfig()
	}
	return &out
}

// configStore 负责配置的读写：读走原子指针（保存后立即生效），写加锁并原子落盘。
type configStore struct {
	path string
	cur  atomic.Pointer[config]
}

func newConfigStore(path string) (*configStore, error) {
	s := &configStore{path: path}
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var loaded config
		if json.Unmarshal(b, &loaded) == nil {
			cfg = &loaded
		}
	case os.IsNotExist(err):
	default:
		return nil, err
	}
	cfg.normalize()
	s.cur.Store(cfg)
	return s, nil
}

func (s *configStore) get() *config { return s.cur.Load() }

func (s *configStore) save(cfg *config) error {
	cfg.Version = cfgVersion
	cfg.normalize()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.cur.Store(cfg)
	return nil
}
