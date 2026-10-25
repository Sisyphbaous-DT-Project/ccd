package main

// 令牌身份、分组目录、auto 分组解析，以及管理接口的管理员校验。
//
// 身份和分组目录都直接只读 new-api 的数据库（newapi.go），这里只做归一化和缓存。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type identity struct {
	Found      bool     `json:"found"`
	UserID     int64    `json:"user_id"`
	TokenGroup string   `json:"token_group"`
	UserGroup  string   `json:"user_group"`
	AutoGroups []string `json:"auto_groups"`
	TokenID    int64    `json:"token_id"`
}

// lookupTimeout 查库查不动时尽快放弃：查身份失败就原样透传，别把用户的请求卡住。
const lookupTimeout = 3 * time.Second

// normalizeKey 把客户端带来的鉴权值规整成 new-api 库里存的 key：
// 去掉 Bearer 前缀、sk- 前缀，以及 new-api 允许的「key-渠道号」这类后缀。
func normalizeKey(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		v = strings.TrimSpace(v[7:])
	}
	v = strings.TrimPrefix(v, "sk-")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	return v
}

type idEntry struct {
	id identity
	at time.Time
}

// identityCache 按 key 认身份，结果（包括「查不到」）缓存 ttl。
type identityCache struct {
	db  *newAPIDB
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]idEntry
}

func newIdentityCache(db *newAPIDB, ttl time.Duration) *identityCache {
	return &identityCache{db: db, ttl: ttl, now: time.Now, cache: map[string]idEntry{}}
}

// Lookup 按令牌认身份。auth 是客户端请求里原样的鉴权值（可带 Bearer / sk- 前缀）。
func (c *identityCache) Lookup(ctx context.Context, auth string) (identity, bool) {
	key := normalizeKey(auth)
	if key == "" {
		return identity{}, false
	}
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.id, e.id.Found
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	id, err := c.db.identity(ctx, key)
	if err != nil {
		log.Printf("relay: identity lookup failed, passing through: %v", err)
		return identity{}, false
	}
	c.mu.Lock()
	if len(c.cache) > 20000 {
		c.cache = map[string]idEntry{}
	}
	c.cache[key] = idEntry{id: id, at: now}
	c.mu.Unlock()
	return id, id.Found
}

type catalogGroup struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type catalog struct {
	Groups []catalogGroup `json:"groups"`

	index map[string]map[string]bool
}

func (c *catalog) build() {
	c.index = make(map[string]map[string]bool, len(c.Groups))
	for _, g := range c.Groups {
		m := make(map[string]bool, len(g.Models))
		for _, name := range g.Models {
			m[name] = true
		}
		c.index[g.Name] = m
	}
}

func (c *catalog) has(group, model string) bool {
	if c == nil || c.index == nil {
		return false
	}
	return c.index[group][model]
}

func (c *catalog) groups() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Groups))
	for _, g := range c.Groups {
		out = append(out, g.Name)
	}
	return out
}

// catalogCache 缓存「分组 → 启用模型」目录 ttl。
type catalogCache struct {
	db  *newAPIDB
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	at   time.Time
	data *catalog
}

func newCatalogCache(db *newAPIDB, ttl time.Duration) *catalogCache {
	return &catalogCache{db: db, ttl: ttl, now: time.Now}
}

func (c *catalogCache) get(ctx context.Context) (*catalog, error) {
	now := c.now()
	c.mu.Lock()
	if c.data != nil && now.Sub(c.at) < c.ttl {
		d := c.data
		c.mu.Unlock()
		return d, nil
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	cat, err := c.db.catalog(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.data, c.at = cat, now
	c.mu.Unlock()
	return cat, nil
}

// resolveGroup 还原这次请求实际会落在哪个分组：令牌分组非 auto 就是它；
// auto 按 new-api 的顺序（令牌 auto_groups，为空用 options 的 AutoGroups）取第一个
// 「该模型在这个分组里有启用渠道」的分组。
func resolveGroup(id identity, model string, cat *catalog) string {
	if id.TokenGroup != "auto" {
		return id.TokenGroup
	}
	for _, g := range id.AutoGroups {
		if cat.has(g, model) {
			return g
		}
	}
	return ""
}

type authEntry struct {
	role int
	at   time.Time
}

// authCache 校验管理接口的管理员身份：拿前端给的 access_token 调 new-api /api/user/self，
// role>=100 才放行，结果缓存 10 分钟。
type authCache struct {
	base   string
	client *http.Client
	now    func() time.Time

	mu sync.Mutex
	m  map[string]authEntry
}

func newAuthCache(base string, client *http.Client) *authCache {
	return &authCache{base: strings.TrimRight(base, "/"), client: client, now: time.Now, m: map[string]authEntry{}}
}

func (a *authCache) allowed(ctx context.Context, token, uid string) bool {
	if token == "" || uid == "" {
		return false
	}
	key := token + "|" + uid
	now := a.now()
	a.mu.Lock()
	if e, ok := a.m[key]; ok && now.Sub(e.at) < 10*time.Minute {
		a.mu.Unlock()
		return e.role >= 100
	}
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/api/user/self", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("New-Api-User", uid)
	resp, err := a.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	role := 0
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Role int `json:"role"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) == nil && out.Success {
		role = out.Data.Role
	}
	a.mu.Lock()
	if len(a.m) > 200 {
		a.m = map[string]authEntry{}
	}
	a.m[key] = authEntry{role: role, at: now}
	a.mu.Unlock()
	return role >= 100
}
