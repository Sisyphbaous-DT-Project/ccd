package main

// 「关思考参数」和高级设置的回归测试：
// 面板的输入框提交上来是一段 JSON 文本（"{\"a\":1}"），直接调接口的给的是对象（{"a":1}），
// 两种都必须收；以前只认对象，面板里填对了也说「必须是 JSON 对象」，整条规则连带存不进去。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNewAPIWithTokens 假的 new-api：/api/user/self 给管理员身份，/api/token/ 列名下 key。
// 管理接口保存时要拿这个身份调这两个接口，缺了就存不进去。
type fakeNewAPIWithTokens struct{}

func (fakeNewAPIWithTokens) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/user/self":
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"role": 100, "id": 1}})
	case strings.HasPrefix(r.URL.Path, "/api/token/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
			"items": []map[string]any{
				{"id": 99, "name": "管理员的key", "group": "normal", "status": 1},
				{"id": 42, "name": "普通用户的key", "group": "normal", "status": 1},
			},
			"total": 2,
		}})
	default:
		w.WriteHeader(404)
	}
}

func TestNormalizeExtraAcceptsBothForms(t *testing.T) {
	obj := `{"reasoning_effort":"off","thinking":{"type":"disabled"}}`
	cases := []struct {
		name string
		in   string
		want string
		bad  bool
	}{
		{name: "面板发来的字符串", in: mustJSON(t, obj), want: obj},
		{name: "直接调的接口给对象", in: obj, want: obj},
		{name: "对象带空白", in: ` { "a" : 1 } `, want: `{"a":1}`},
		{name: "字符串里带空白", in: mustJSON(t, `  {"a":1}  `), want: `{"a":1}`},
		{name: "留空", in: "", want: ""},
		{name: "空字符串", in: `""`, want: ""},
		{name: "null", in: "null", want: ""},
		{name: "空对象", in: "{}", want: ""},
		{name: "不是 JSON", in: `{"a":}`, bad: true},
		{name: "字符串里不是 JSON", in: mustJSON(t, "not json"), bad: true},
		{name: "数组也不行", in: "[1,2]", bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &advanced{Extra: json.RawMessage(c.in)}
			err := a.normalizeExtra()
			if c.bad {
				if err == nil {
					t.Fatalf("想要报错，结果通过了：%s", a.Extra)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if c.want == "" {
				if len(a.Extra) != 0 {
					t.Fatalf("想要留空，得到 %s", a.Extra)
				}
				return
			}
			var got, want map[string]any
			if json.Unmarshal(a.Extra, &got) != nil {
				t.Fatalf("存下来的不是 JSON 对象：%s", a.Extra)
			}
			if json.Unmarshal([]byte(c.want), &want) != nil {
				t.Fatalf("测试用例写错了：%s", c.want)
			}
			if len(got) != len(want) {
				t.Fatalf("解析结果不对：%s，想要 %s", a.Extra, c.want)
			}
			for k, v := range want {
				if got[k] == nil || !equalJSON(got[k], v) {
					t.Fatalf("%s 对不上：%s", k, a.Extra)
				}
			}
		})
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// panelShapedRule 是面板「保存」实际发出去的东西：数字可能全是 0（用户没改），
// extra 是输入框里的文本，所以是一段 JSON 字符串。
func panelShapedRule(extra any) string {
	adv := map[string]any{
		"stream_first": 0, "stream_step": 0, "stream_max": 0,
		"sync_first": 0, "sync_step": 0, "sync_max": 0,
		"parallel": 0, "sync_parallel": 0, "timeout_sec": 0, "content_wait_sec": 0,
	}
	if extra != nil {
		adv["extra"] = extra
	}
	body, _ := json.Marshal(map[string]any{"rules": []any{map[string]any{
		"id": "r1", "enabled": true, "group": "normal", "models": []string{}, "lang": "zh",
		"translator_token_id": 99, "translator_model": "deepseek-v4.1-flash",
		"advanced": adv,
	}}})
	return string(body)
}

// TestAdminConfigSavesPanelPayload 走一遍「点保存 → 关掉面板 → 重进」：
// 保存必须成功，重新读回来的高级设置必须是规整后的值，关思考参数必须还在。
func TestAdminConfigSavesPanelPayload(t *testing.T) {
	up := newFakeUpstream()
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	apiSrv := httptest.NewServer(fakeNewAPIWithTokens{})
	t.Cleanup(apiSrv.Close)

	state := t.TempDir()
	a, err := newApp(appOptions{
		listen: "127.0.0.1:0", upstream: upSrv.URL, summaryURL: upSrv.URL + "/v1/chat/completions",
		newAPIBase: apiSrv.URL, sqlitePath: newTestSQLite(t), stateDir: state,
		storeTTL: 24 * 3600 * 1e9, storeMax: 8 << 20,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { a.db.Close() })
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)

	admin := func(method, path, body string) (int, map[string]any) {
		var rd *strings.Reader
		if body != "" {
			rd = strings.NewReader(body)
		} else {
			rd = strings.NewReader("")
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("X-NewAPI-Token", "admintoken")
		req.Header.Set("X-NewAPI-User", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if json.NewDecoder(resp.Body).Decode(&out) != nil {
			t.Fatalf("%s %s: 返回的不是 JSON", method, path)
		}
		return resp.StatusCode, out
	}

	extra := `{"reasoning_effort":"off","thinking":{"type":"disabled"}}`
	code, out := admin(http.MethodPut, "/ccd/admin/config", panelShapedRule(extra))
	if code != http.StatusOK || out["success"] != true {
		t.Fatalf("面板的保存被拒了：%d %v", code, out["message"])
	}

	// 重新读一遍 = 关掉面板再点进来
	code, out = admin(http.MethodGet, "/ccd/admin/config", "")
	if code != http.StatusOK {
		t.Fatalf("读配置失败：%d", code)
	}
	rules, _ := out["data"].(map[string]any)["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("规则数量不对：%v", rules)
	}
	adv, _ := rules[0].(map[string]any)["advanced"].(map[string]any)
	if adv["stream_first"] != float64(100) || adv["parallel"] != float64(3) || adv["timeout_sec"] != float64(20) {
		t.Fatalf("高级参数没有被拉回默认值：%v", adv)
	}
	got, ok := adv["extra"].(map[string]any)
	if !ok || got["reasoning_effort"] != "off" {
		t.Fatalf("关思考参数没存住：%v", adv["extra"])
	}
	if think, _ := got["thinking"].(map[string]any); think == nil || think["type"] != "disabled" {
		t.Fatalf("关思考参数的 thinking 丢了：%v", got)
	}

	// 重启（新开一次 configStore 读同一个文件）后还得在
	cfg, err := newConfigStore(filepath.Join(state, "rules.json"))
	if err != nil {
		t.Fatalf("newConfigStore: %v", err)
	}
	reloaded := cfg.get()
	if len(reloaded.Rules) != 1 || reloaded.Rules[0].Advanced == nil {
		t.Fatalf("重启后规则丢了：%+v", reloaded.Rules)
	}
	var persisted map[string]any
	if json.Unmarshal(reloaded.Rules[0].Advanced.Extra, &persisted) != nil || persisted["reasoning_effort"] != "off" {
		t.Fatalf("重启后关思考参数不对：%s", reloaded.Rules[0].Advanced.Extra)
	}
	if got := reloaded.Rules[0].Advanced.extraFields(); got["reasoning_effort"] != "off" {
		t.Fatalf("重启后实际用到转述调用上的参数不对：%v", got)
	}
}

// TestConfigStoreNormalizesDisabledExtra 停用中的规则（半成品）存盘时，
// 关思考参数也要收成对象；填坏的退回默认，别把一段坏文本留在盘上。
func TestConfigStoreNormalizesDisabledExtra(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  map[string]any // 落盘后应为的对象；nil 表示不该存在
	}{
		{"面板发来的字符串", `{"reasoning_effort":"off"}`, map[string]any{"reasoning_effort": "off"}},
		{"直接给的对象", `{"reasoning_effort":"high"}`, map[string]any{"reasoning_effort": "high"}},
		{"填坏了退回默认", `{"a":}`, nil},
		{"留空", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rules.json")
			s, err := newConfigStore(path)
			if err != nil {
				t.Fatalf("newConfigStore: %v", err)
			}
			// 停用 + 没选转述 key：校验会在读到高级设置之前就返回，正是「半成品」那种情况
			cfg := &config{Rules: []rule{{
				ID: "r1", Enabled: false, Group: "normal", Lang: langZH,
				Advanced: &advanced{Extra: json.RawMessage(mustJSON(t, c.extra))},
			}}}
			if err := s.save(cfg); err != nil {
				t.Fatalf("save: %v", err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读落盘文件: %v", err)
			}
			var onDisk struct {
				Rules []struct {
					Advanced *struct {
						Extra json.RawMessage `json:"extra"`
					} `json:"advanced"`
				} `json:"rules"`
			}
			if err := json.Unmarshal(b, &onDisk); err != nil {
				t.Fatalf("落盘的不是 JSON：%v", err)
			}
			raw := onDisk.Rules[0].Advanced.Extra
			if c.want == nil {
				if s := string(bytes.TrimSpace(raw)); s != "" && s != "null" {
					t.Fatalf("坏值不该留在盘上，得到 %s", raw)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("落盘的不是 JSON 对象：%s", raw)
			}
			if !equalJSON(got, c.want) {
				t.Fatalf("落盘内容不对：%v，想要 %v", got, c.want)
			}
		})
	}
}

// TestAdminConfigKeepsCustomAdvanced 用户改了高级参数再保存，重进时必须是改后的值，不能回退成 0 或默认值。
func TestAdminConfigKeepsCustomAdvanced(t *testing.T) {
	up := newFakeUpstream()
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	apiSrv := httptest.NewServer(fakeNewAPIWithTokens{})
	t.Cleanup(apiSrv.Close)

	state := t.TempDir()
	a, err := newApp(appOptions{
		listen: "127.0.0.1:0", upstream: upSrv.URL, summaryURL: upSrv.URL + "/v1/chat/completions",
		newAPIBase: apiSrv.URL, sqlitePath: newTestSQLite(t), stateDir: state,
		storeTTL: 24 * 3600 * 1e9, storeMax: 8 << 20,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { a.db.Close() })
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)

	call := func(method, body string) map[string]any {
		req, _ := http.NewRequest(method, srv.URL+"/ccd/admin/config", strings.NewReader(body))
		req.Header.Set("X-NewAPI-Token", "admintoken")
		req.Header.Set("X-NewAPI-User", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if json.NewDecoder(resp.Body).Decode(&out) != nil {
			t.Fatalf("%s: 返回的不是 JSON", method)
		}
		if out["success"] != true {
			t.Fatalf("%s 失败：%v", method, out["message"])
		}
		return out
	}

	wantNums := map[string]int{
		"stream_first": 300, "stream_step": 120, "stream_max": 1500,
		"sync_first": 400, "sync_step": 90, "sync_max": 1200,
		"parallel": 5, "sync_parallel": 9, "timeout_sec": 45, "content_wait_sec": 30,
	}
	adv := map[string]any{}
	for k, v := range wantNums {
		adv[k] = v
	}
	adv["extra"] = `{"reasoning_effort":"low"}`
	body, _ := json.Marshal(map[string]any{"rules": []any{map[string]any{
		"id": "r1", "enabled": true, "group": "normal", "models": []string{}, "lang": "zh",
		"translator_token_id": 99, "translator_model": "deepseek-v4.1-flash", "advanced": adv,
	}}})
	call(http.MethodPut, string(body))

	// 重新读 = 关掉面板重进
	got := call(http.MethodGet, "")
	rules := got["data"].(map[string]any)["rules"].([]any)
	gadv := rules[0].(map[string]any)["advanced"].(map[string]any)
	for k, v := range wantNums {
		if gadv[k] != float64(v) {
			t.Fatalf("%s 变成 %v，想要 %d（整份：%v）", k, gadv[k], v, gadv)
		}
	}
	if e, _ := gadv["extra"].(map[string]any); e == nil || e["reasoning_effort"] != "low" {
		t.Fatalf("关思考参数变成 %v", gadv["extra"])
	}
}

// TestPanelNeverScrapesAdvancedFromDOM 面板不能在保存时从 DOM 刮高级参数：
// 高级设置收起后那些输入框不在文档里，刮出来就是 0 —— 「保存后重进，之前设过的全变 0」就是这么来的。
// 这里直接读面板源码卡住这个约定，避免以后又被改回去。
func TestPanelNeverScrapesAdvancedFromDOM(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("web", "ccd-panel.js"))
	if err != nil {
		t.Fatalf("读面板源码: %v", err)
	}
	js := string(src)
	// collectEditor 里不能再出现按 id 读输入框的代码
	start := strings.Index(js, "function collectEditor()")
	if start < 0 {
		t.Fatal("找不到 collectEditor")
	}
	end := strings.Index(js[start:], "\n  async function saveRule")
	if end < 0 {
		t.Fatal("找不到 collectEditor 的结尾")
	}
	body := js[start : start+end]
	// 高级设置那几个框的 id 一个都不能出现（其它字段一直渲染着，按 id 读没问题）
	for _, id := range []string{"ccd-sf", "ccd-ss", "ccd-sm", "ccd-nf", "ccd-ns", "ccd-nm",
		"ccd-p1", "ccd-p2", "ccd-to", "ccd-cw", "ccd-ex"} {
		if strings.Contains(body, `"`+id+`"`) {
			t.Fatalf("collectEditor 又开始从 DOM 读 %s 了，收起的高级设置会被读成 0：\n%s", id, body)
		}
	}
	// 输入框必须带 data-adv，值随输入写回规则
	for _, k := range []string{"stream_first", "stream_step", "stream_max", "sync_first", "sync_step",
		"sync_max", "parallel", "sync_parallel", "timeout_sec", "content_wait_sec"} {
		if !strings.Contains(js, `data-adv="`+k+`"`) {
			t.Fatalf("高级设置里的 %s 没有 data-adv，改动不会写回规则", k)
		}
	}
	if !strings.Contains(js, `data-adv="extra"`) {
		t.Fatal("关思考参数的输入框没有 data-adv")
	}
}

// TestAdminConfigRejectsBrokenExtra 填错的东西要报错，而不是悄悄存个默认值进去。
func TestAdminConfigRejectsBrokenExtra(t *testing.T) {
	up := newFakeUpstream()
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	apiSrv := httptest.NewServer(fakeNewAPIWithTokens{})
	t.Cleanup(apiSrv.Close)

	state := t.TempDir()
	a, err := newApp(appOptions{
		listen: "127.0.0.1:0", upstream: upSrv.URL, summaryURL: upSrv.URL + "/v1/chat/completions",
		newAPIBase: apiSrv.URL, sqlitePath: newTestSQLite(t), stateDir: state,
		storeTTL: 24 * 3600 * 1e9, storeMax: 8 << 20,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { a.db.Close() })
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/ccd/admin/config", strings.NewReader(panelShapedRule("[1,2]")))
	req.Header.Set("X-NewAPI-Token", "admintoken")
	req.Header.Set("X-NewAPI-User", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("想要 400，得到 %d %v", resp.StatusCode, out["message"])
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "JSON 对象") {
		t.Fatalf("错误信息没说清楚：%q", msg)
	}
	if _, err := os.Stat(filepath.Join(state, "rules.json")); err == nil {
		t.Fatalf("校验没通过就不该落盘")
	}
}
