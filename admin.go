package main

// 管理接口：规则目录、规则读写、用示例思维链试跑一次转述。
//
// 鉴权：拿前端给的 X-NewAPI-Token / X-NewAPI-User 调 new-api 的 /api/user/self，
// role>=100 才放行，否则 403 {"success":false,"message":"root only"}。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Sisyphbaous-DT-Project/ccd/internal/thinkhide"
)

const sampleExcerpt = `The request asks for a small CLI that renames files in a directory based on a pattern, with a dry-run flag. I'll check whether the repo already has an argument parser — yes, cobra is vendored in cmd/root.go, so I'll add a subcommand instead of writing flag parsing by hand.

Next I need the rename plan itself: walk the directory, skip subdirectories, keep the extension, and build old→new pairs; if two targets collide I should abort before touching anything. For dry-run I'll print the same plan and return early. After that I'll wire up a test with t.TempDir() covering the collision path.`

type adminResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func writeAdminJSON(w http.ResponseWriter, code int, v adminResponse) {
	b, err := json.Marshal(v)
	if err != nil {
		code = http.StatusInternalServerError
		b = []byte(`{"success":false,"message":"encode error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (a *app) adminAuthorized(r *http.Request) bool {
	return a.auth.allowed(r.Context(), r.Header.Get("X-NewAPI-Token"), r.Header.Get("X-NewAPI-User"))
}

func (a *app) adminCatalog(w http.ResponseWriter, r *http.Request) {
	if !a.adminAuthorized(r) {
		writeAdminJSON(w, http.StatusForbidden, adminResponse{Message: "root only"})
		return
	}
	cat, err := a.cat.get(r.Context())
	if err != nil {
		writeAdminJSON(w, http.StatusBadGateway, adminResponse{Message: "读取分组目录失败：" + err.Error()})
		return
	}
	writeAdminJSON(w, http.StatusOK, adminResponse{Success: true, Data: cat})
}

func (a *app) adminConfig(w http.ResponseWriter, r *http.Request) {
	if !a.adminAuthorized(r) {
		writeAdminJSON(w, http.StatusForbidden, adminResponse{Message: "root only"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := a.cfg.get()
		writeAdminJSON(w, http.StatusOK, adminResponse{Success: true, Data: map[string]any{
			"rules":            cfg.Rules,
			"default_advanced": defaultAdvanced(),
		}})
	case http.MethodPut:
		a.saveConfig(w, r)
	default:
		writeAdminJSON(w, http.StatusMethodNotAllowed, adminResponse{Message: "method not allowed"})
	}
}

func defaultAdvanced() advanced {
	a := advanced{}
	a.clamp()
	a.Extra = json.RawMessage(`{"reasoning_effort":"off","thinking":{"type":"disabled"}}`)
	return a
}

func (a *app) saveConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: "读取请求失败"})
		return
	}
	var incoming config
	if err := json.Unmarshal(body, &incoming); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: "配置格式不对：" + err.Error()})
		return
	}
	incoming.Version = cfgVersion
	for i := range incoming.Rules {
		ru := &incoming.Rules[i]
		if ru.ID == "" {
			ru.ID = fmt.Sprintf("r%d", i+1)
		}
		// 停用中的规则允许先存半成品（比如还没选转述用的 key），启用时才严格校验。
		if err := ru.validate(); err != nil && ru.Enabled {
			writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: err.Error()})
			return
		}
	}
	seen := map[string]bool{}
	for i := range incoming.Rules {
		ru := &incoming.Rules[i]
		if ru.ID == "" || seen[ru.ID] {
			ru.ID = fmt.Sprintf("r%d-%d", i+1, time.Now().UnixNano()%100000)
		}
		seen[ru.ID] = true
	}

	// 「按 key 套转述」的 key 和转述用的 key 都必须是你名下现有的；名字、转述分组以服务器查到的为准。
	mine, err := a.listTokens(r.Context(), r.Header.Get("X-NewAPI-Token"), r.Header.Get("X-NewAPI-User"))
	if err != nil {
		writeAdminJSON(w, http.StatusBadGateway, adminResponse{Message: "读取你的 key 列表失败：" + err.Error()})
		return
	}
	byID := make(map[int64]tokenInfo, len(mine))
	for _, t := range mine {
		byID[t.ID] = t
	}
	for i := range incoming.Rules {
		ru := &incoming.Rules[i]
		if ru.Scope == scopeToken && ru.TokenID > 0 {
			t, ok := byID[ru.TokenID]
			if !ok && ru.Enabled {
				writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: fmt.Sprintf("要套转述的 key（#%d）不在你名下或已删除", ru.TokenID)})
				return
			}
			if ok {
				ru.TokenName = t.Name
			}
		}
		if ru.TranslatorTokenID <= 0 {
			continue
		}
		t, ok := byID[ru.TranslatorTokenID]
		if !ok {
			if ru.Enabled {
				writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: fmt.Sprintf("转述用的 key（#%d）不在你名下或已删除", ru.TranslatorTokenID)})
				return
			}
			continue
		}
		ru.TranslatorTokenName = t.Name
		_, group, err := a.db.tokenKey(r.Context(), ru.TranslatorTokenID)
		if err != nil {
			if ru.Enabled {
				writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: fmt.Sprintf("转述用的 key「%s」现在用不了：%v", t.Name, err)})
				return
			}
			continue
		}
		ru.TranslatorGroup = group
		if ru.Enabled && group == "auto" {
			writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: "转述用的 key 不能是 auto 分组的，请换一个固定分组的 key"})
			return
		}
	}

	if cat, err := a.cat.get(r.Context()); err == nil {
		for _, ru := range incoming.Rules {
			if len(cat.index) == 0 {
				break
			}
			if !ru.Enabled {
				continue
			}
			if _, ok := cat.index[ru.Group]; ru.Scope != scopeToken && !ok {
				writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: fmt.Sprintf("分组「%s」现在没有启用的渠道", ru.Group)})
				return
			}
			if !cat.has(ru.TranslatorGroup, ru.TranslatorModel) {
				writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: fmt.Sprintf("转述模型「%s」不在分组「%s」里", ru.TranslatorModel, ru.TranslatorGroup)})
				return
			}
		}
	}

	if err := a.cfg.save(&incoming); err != nil {
		writeAdminJSON(w, http.StatusInternalServerError, adminResponse{Message: "保存失败：" + err.Error()})
		return
	}
	writeAdminJSON(w, http.StatusOK, adminResponse{Success: true, Data: map[string]any{"rules": incoming.Rules}})
}

// tokenInfo 是给面板选令牌用的精简信息：只有 id、名字、分组、状态，绝不带 key。
type tokenInfo struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Group  string `json:"group"`
	Status int    `json:"status"`
}

// listTokens 用当前登录的 root 身份调 new-api 官方 GET /api/token/，列出他名下的 key。key 字段不解析、不下发。
func (a *app) listTokens(ctx context.Context, authToken, uid string) ([]tokenInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := make([]tokenInfo, 0, 32)
	for page := 1; page <= 50; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/token/?p=%d&size=100", a.apiBase, page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("New-Api-User", uid)
		resp, err := a.client.Do(req)
		if err != nil {
			return nil, err
		}
		var body struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
			Data    struct {
				Items []tokenInfo `json:"items"`
				Total int         `json:"total"`
			} `json:"data"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if !body.Success {
			return nil, errors.New(body.Message)
		}
		out = append(out, body.Data.Items...)
		if len(body.Data.Items) == 0 || len(out) >= body.Data.Total {
			break
		}
	}
	return out, nil
}

// adminTokens 给面板列出你名下的 key（选「按 key 套转述」和「转述用的 key」都用它）。
func (a *app) adminTokens(w http.ResponseWriter, r *http.Request) {
	if !a.adminAuthorized(r) {
		writeAdminJSON(w, http.StatusForbidden, adminResponse{Message: "root only"})
		return
	}
	list, err := a.listTokens(r.Context(), r.Header.Get("X-NewAPI-Token"), r.Header.Get("X-NewAPI-User"))
	if err != nil {
		writeAdminJSON(w, http.StatusBadGateway, adminResponse{Message: "读取 key 列表失败：" + err.Error()})
		return
	}
	writeAdminJSON(w, http.StatusOK, adminResponse{Success: true, Data: list})
}

type testRequest struct {
	Lang              string    `json:"lang"`
	LangCustom        string    `json:"lang_custom"`
	TranslatorGroup   string    `json:"translator_group"`
	TranslatorModel   string    `json:"translator_model"`
	TranslatorTokenID int64     `json:"translator_token_id"`
	Advanced          *advanced `json:"advanced"`
}

func (a *app) adminTest(w http.ResponseWriter, r *http.Request) {
	if !a.adminAuthorized(r) {
		writeAdminJSON(w, http.StatusForbidden, adminResponse{Message: "root only"})
		return
	}
	if r.Method != http.MethodPost {
		writeAdminJSON(w, http.StatusMethodNotAllowed, adminResponse{Message: "method not allowed"})
		return
	}
	var req testRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: "请求格式不对"})
		return
	}
	ru := rule{
		Lang: req.Lang, LangCustom: req.LangCustom,
		TranslatorGroup: req.TranslatorGroup, TranslatorModel: req.TranslatorModel,
		TranslatorTokenID: req.TranslatorTokenID,
		Advanced:          req.Advanced,
	}
	if err := ru.validateTranslator(); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: err.Error()})
		return
	}
	ps := promptFor(ru.Lang, ru.LangCustom)
	adv := ru.Advanced
	extra := adv.extraFields()
	key, _, err := a.db.tokenKey(r.Context(), ru.TranslatorTokenID)
	if err != nil {
		writeAdminJSON(w, http.StatusBadRequest, adminResponse{Message: "转述用的 key 用不了：" + err.Error()})
		return
	}
	system := ps.System
	ctx := thinkhide.WithSegment(r.Context(), 0, 7)
	user := makeWrap(ps, &recentNotes{})(ctx, sampleExcerpt)
	payload := map[string]any{
		"model":       ru.TranslatorModel,
		"messages":    []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": user}},
		"stream":      false,
		"max_tokens":  ps.MaxTokens,
		"temperature": 0.5,
	}
	for k, v := range extra {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	hreq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.summaryURL, bytes.NewReader(b))
	if err != nil {
		writeAdminJSON(w, http.StatusInternalServerError, adminResponse{Message: err.Error()})
		return
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer sk-"+key)
	start := time.Now()
	resp, err := a.client.Do(hreq)
	if err != nil {
		writeAdminJSON(w, http.StatusBadGateway, adminResponse{Message: "调用转述模型失败：" + err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		writeAdminJSON(w, http.StatusBadGateway, adminResponse{Message: fmt.Sprintf("转述模型返回 %d：%s", resp.StatusCode, clip(string(body), 300))})
		return
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			CompletionTokenDetl struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		writeAdminJSON(w, http.StatusBadGateway, adminResponse{Message: "转述模型返回的不是 JSON：" + clip(string(body), 300)})
		return
	}
	text := ""
	if len(out.Choices) > 0 {
		text = strings.TrimSpace(out.Choices[0].Message.Content)
	}
	writeAdminJSON(w, http.StatusOK, adminResponse{Success: true, Data: map[string]any{
		"text":              text,
		"ms":                elapsed.Milliseconds(),
		"prompt_tokens":     out.Usage.PromptTokens,
		"completion_tokens": out.Usage.CompletionTokens,
		"reasoning_tokens":  out.Usage.CompletionTokenDetl.ReasoningTokens,
	}})
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
