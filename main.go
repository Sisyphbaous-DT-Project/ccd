// ccd：给 new-api 的任意分组 / 模型 / key 套上「思维链转述」的外挂服务。
//
// 位置：反向代理（如 nginx）的模型入口 → ccd → new-api。
// 要求只有一句话：命中规则时，客户端看到的思维链文字换成转述，
// 正文、工具调用、usage、id、model、finish_reason、签名、其它所有字段和事件顺序都不动；
// 没命中规则的请求和响应逐字节原样透传。
package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	version = "0.1.0"

	kindChat      = "chat"
	kindAnthropic = "messages"

	maxReqBody  = 64 << 20
	maxRespBody = 64 << 20
)

var errResponseTooLarge = errors.New("response too large")

// panelJS 是注入 new-api 页面的管理面板脚本，打包进程序，由 /ccd/panel.js 提供。
//
//go:embed web/ccd-panel.js
var panelJS []byte

type app struct {
	listen     string
	upstream   *url.URL
	summaryURL string
	apiBase    string
	client     *http.Client
	store      *diskStore
	cfg        *configStore
	db         *newAPIDB
	ident      *identityCache
	cat        *catalogCache
	auth       *authCache
	proxy      *httputil.ReverseProxy

	keysMu sync.Mutex
	keys   map[int64]keyEntry // 转述用 key 的缓存：令牌 id -> key（随用随取，不落盘）
}

type appOptions struct {
	listen     string
	upstream   string
	summaryURL string
	newAPIBase string
	sqlDSN     string
	sqlitePath string
	stateDir   string
	storeTTL   time.Duration
	storeMax   int64
}

func defaultOptions() appOptions {
	newAPI := strings.TrimRight(getenv("CCD_NEWAPI", "http://127.0.0.1:3000"), "/")
	return appOptions{
		listen:     getenv("CCD_LISTEN", "127.0.0.1:3140"),
		upstream:   getenv("CCD_UPSTREAM", newAPI),
		summaryURL: getenv("CCD_SUMMARY_URL", newAPI+"/v1/chat/completions"),
		newAPIBase: newAPI,
		sqlDSN:     os.Getenv("CCD_SQL_DSN"),
		sqlitePath: os.Getenv("CCD_SQLITE_PATH"),
		stateDir:   getenv("CCD_STATE", "/var/lib/ccd"),
		storeTTL:   time.Duration(envInt("CCD_STORE_TTL_DAYS", 30)) * 24 * time.Hour,
		storeMax:   int64(envInt("CCD_STORE_MAX_MB", 512)) << 20,
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func newApp(opts appOptions) (*app, error) {
	upstream, err := url.Parse(opts.upstream)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.stateDir, 0o700); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 0}
	store, err := newDiskStore(filepath.Join(opts.stateDir, "store"), opts.storeTTL, opts.storeMax)
	if err != nil {
		return nil, err
	}
	cfg, err := newConfigStore(filepath.Join(opts.stateDir, "rules.json"))
	if err != nil {
		return nil, err
	}
	db, err := openNewAPIDB(opts.sqlDSN, opts.sqlitePath)
	if err != nil {
		return nil, err
	}
	a := &app{
		listen:     opts.listen,
		upstream:   upstream,
		summaryURL: opts.summaryURL,
		apiBase:    strings.TrimRight(opts.newAPIBase, "/"),
		client:     client,
		store:      store,
		cfg:        cfg,
		db:         db,
		ident:      newIdentityCache(db, 60*time.Second),
		cat:        newCatalogCache(db, 60*time.Second),
		auth:       newAuthCache(opts.newAPIBase, client),
		keys:       map[int64]keyEntry{},
	}
	a.proxy = a.newProxy()
	return a, nil
}

// keyEntry 是转述用 key 的缓存条目。
type keyEntry struct {
	key, group string
	at         time.Time
}

// keyTTL 转述用 key 随用随取、只缓存 5 分钟：管理员在后台删掉或停用这个 key 后，最多 5 分钟就不再用它。
const keyTTL = 5 * time.Minute

// translatorKey 按令牌 id 取转述用的 key 和它实际所在的分组（查 new-api 数据库，结果不落盘）。
func (a *app) translatorKey(ctx context.Context, tokenID int64) (key, group string, err error) {
	if tokenID <= 0 {
		return "", "", errors.New("没有选转述用的 key")
	}
	a.keysMu.Lock()
	if e, ok := a.keys[tokenID]; ok && time.Since(e.at) < keyTTL {
		a.keysMu.Unlock()
		return e.key, e.group, nil
	}
	a.keysMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	key, group, err = a.db.tokenKey(ctx, tokenID)
	if err != nil {
		return "", "", err
	}
	a.keysMu.Lock()
	if len(a.keys) > 1000 {
		a.keys = map[int64]keyEntry{}
	}
	a.keys[tokenID] = keyEntry{key: key, group: group, at: time.Now()}
	a.keysMu.Unlock()
	return key, group, nil
}

func (a *app) newProxy() *httputil.ReverseProxy {
	upstream := a.upstream
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableCompression = true
	tr.ResponseHeaderTimeout = 3600 * time.Second
	tr.MaxIdleConnsPerHost = 64
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = pr.In.Host // 上游看到的 Host 必须还是客户端那个，否则 new-api 的站内链接/校验会变
			// Rewrite 模式会先删掉 Forwarded / X-Forwarded-*：把前面反代填好的原样抄回去（不调用 SetXForwarded，
			// 免得再追加一跳），new-api 看到的转发头和反代直连它时一模一样（记录的用户 IP、令牌 IP 白名单都不受影响）。
			for _, h := range forwardedHeaders {
				if v, ok := pr.In.Header[h]; ok {
					pr.Out.Header[h] = v
				}
			}
		},
		Transport:      tr,
		FlushInterval:  -1,
		ModifyResponse: a.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			log.Printf("relay: upstream error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"local gateway unavailable","type":"gateway_error"}}`)
		},
	}
}

// forwardedHeaders 是 ReverseProxy 的 Rewrite 模式会自动删掉的几个转发头。
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// relayPath 只有这两个端点参与转述，其余（/v1/responses、Gemini 原生、embeddings、rerank…）一律原样透传。
func relayPath(p string) bool {
	return p == "/v1/chat/completions" || p == "/v1/messages"
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	// 管理接口和面板脚本都挂在 /ccd/ 下面：反代只要把 /ccd/ 整个转给 ccd 就行。
	mux.HandleFunc("/ccd/admin/config", a.adminConfig)
	mux.HandleFunc("/ccd/admin/catalog", a.adminCatalog)
	mux.HandleFunc("/ccd/admin/test", a.adminTest)
	mux.HandleFunc("/ccd/admin/tokens", a.adminTokens)
	mux.HandleFunc("/ccd/panel.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(panelJS)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/", a.relay)
	return mux
}

func (a *app) relay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !relayPath(r.URL.Path) || r.Header.Get("Content-Encoding") != "" {
		a.proxy.ServeHTTP(w, r)
		return
	}
	id, ok := a.ident.Lookup(r.Context(), tokenFromRequest(r))
	if !ok || id.UserID <= 0 {
		a.proxy.ServeHTTP(w, r)
		return
	}
	cfg := a.cfg.get()
	candidates := []string{id.TokenGroup}
	if id.TokenGroup == "auto" {
		candidates = id.AutoGroups
	}
	needsRules := cfg.tokenHasRules(id.TokenID)
	for _, g := range candidates {
		if needsRules {
			break
		}
		needsRules = cfg.groupHasRules(g)
	}
	hasHistory := a.store.Has(id.UserID)
	if !needsRules && !hasHistory {
		// 这个令牌碰不到任何规则、也没收到过转述：连请求体都不解析，原样透传。
		a.proxy.ServeHTTP(w, r)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReqBody+1))
	if err != nil || len(raw) > maxReqBody {
		log.Printf("relay: request body too large or unreadable (%d bytes), passing through", len(raw))
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
		r.ContentLength = -1
		r.Header.Del("Content-Length")
		a.proxy.ServeHTTP(w, r)
		return
	}

	kind := kindChat
	if r.URL.Path == "/v1/messages" {
		kind = kindAnthropic
	}
	var out []byte
	var model string
	var prefix chatPrefix
	restored := 0
	if kind == kindChat {
		out, model, prefix, restored = prepareChat(raw, id.UserID, a.store)
	} else {
		out, model, restored = prepareAnthropic(raw, id.UserID, a.store)
	}

	var hit *rule
	group := ""
	if needsRules {
		// 指定令牌的规则最优先：这个令牌发的请求全部套上转述，跟分组无关。
		if hit = cfg.matchToken(id.TokenID); hit != nil {
			group = id.TokenGroup
		} else {
			cat, err := a.cat.get(r.Context())
			if err != nil {
				log.Printf("relay: catalog unavailable, skipping rules: %v", err)
			}
			group = resolveGroup(id, model, cat)
			if group != "" {
				hit = cfg.match(group, model)
			}
		}
	}

	ctx := r.Context()
	if hit != nil {
		// 要改写的回复必须是未压缩的：上游要是按客户端的 Accept-Encoding 压缩了，ccd 读不懂就只能原样放行，
		// 原版思维链就漏出去了。去掉这个头只影响传输压缩，不影响内容。
		r.Header.Del("Accept-Encoding")
		ctx = context.WithValue(ctx, relayKey{}, &relayState{
			kind: kind, rule: hit, uid: id.UserID, group: group, model: model, prefix: prefix,
		})
	}
	if restored > 0 {
		log.Printf("relay: %s restored %d original thinking block(s) for user %d", kind, restored, id.UserID)
	}
	r = r.WithContext(ctx)

	if len(out) == len(raw) && bytes.Equal(out, raw) {
		r.Body = io.NopCloser(bytes.NewReader(raw))
	} else {
		r.Body = io.NopCloser(bytes.NewReader(out))
		r.ContentLength = int64(len(out))
		r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	}
	a.proxy.ServeHTTP(w, r)
}

func (a *app) modifyResponse(resp *http.Response) error {
	st := stateFrom(resp.Request.Context())
	if st == nil {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("relay: %s status=%d, passing through untouched", st.kind, resp.StatusCode)
		return nil
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		log.Printf("relay: %s response is %q encoded, passing through untouched", st.kind, enc)
		return nil
	}
	stream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if st.kind == kindAnthropic {
		if stream {
			resp.Body = a.anthropicStream(resp, st)
			resp.Header.Del("Content-Length")
			resp.ContentLength = -1
			return nil
		}
		return a.anthropicNonStream(resp, st)
	}
	if stream {
		resp.Body = a.chatStream(resp, st)
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return nil
	}
	return a.chatNonStream(resp, st)
}

type pipeBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (p *pipeBody) Close() error {
	p.cancel()
	return p.PipeReader.Close()
}

// countingWriter 记下已经写出多少字节。思维链被整段吞掉时，pumpChat 会在上游开始产出的那一刻
// 先放一条只含空格的帧，让客户端尽早收到首包（ccd 在 new-api 前面，new-api 记的首字时间不受这里影响）。
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.n += int64(len(p))
	}
	return c.w.Write(p)
}

func readBody(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody+1))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > maxRespBody {
		return nil, errResponseTooLarge
	}
	return body, nil
}

func setBody(resp *http.Response, body []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("ETag")
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	opts := defaultOptions()
	a, err := newApp(opts)
	if err != nil {
		log.Fatalf("relay: init failed: %v", err)
	}
	go func() {
		for {
			time.Sleep(time.Hour)
			a.store.Cleanup()
		}
	}()
	srv := &http.Server{
		Addr:              opts.listen,
		Handler:           a.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("ccd %s listening on %s -> %s (summary %s)", version, opts.listen, opts.upstream, opts.summaryURL)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
