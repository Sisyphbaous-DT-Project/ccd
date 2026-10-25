package main

// new-api 数据库这一层的测试：SQLite 每次都跑；MySQL / PostgreSQL 设了环境变量
// CCD_TEST_MYSQL_DSN / CCD_TEST_PG_DSN 才跑（会在库里建、删 tokens / users / abilities / options 四张表，请用空的测试库）。

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// schemaFor 按方言建一份精简的 new-api 表结构（只含 ccd 用到的列）。
func schemaFor(d *newAPIDB, withAutoGroups bool) []string {
	deleted, enabled := "DATETIME NULL", "BOOLEAN"
	switch d.dialect {
	case dialectMySQL:
		enabled = "TINYINT(1)"
	case dialectPostgres:
		deleted = "TIMESTAMP NULL"
	}
	auto := ""
	if withAutoGroups {
		auto = ", auto_groups VARCHAR(512)"
	}
	return []string{
		d.prepare(`CREATE TABLE users (id BIGINT PRIMARY KEY, {group} VARCHAR(64), status INT, role INT, deleted_at ` + deleted + `)`),
		d.prepare(`CREATE TABLE tokens (id BIGINT PRIMARY KEY, user_id BIGINT, {key} VARCHAR(64), name VARCHAR(64), {group} VARCHAR(64)` + auto +
			`, status INT, expired_time BIGINT, deleted_at ` + deleted + `)`),
		d.prepare(`CREATE TABLE abilities ({group} VARCHAR(64), model VARCHAR(191), channel_id BIGINT, enabled ` + enabled + `)`),
		d.prepare(`CREATE TABLE options ({key} VARCHAR(191) PRIMARY KEY, value TEXT)`),
	}
}

// seedNewAPI 建表并写入测试数据。raw 是可写连接，d 只用来拿方言（引号、占位符）。
func seedNewAPI(t *testing.T, raw *sql.DB, d *newAPIDB, withAutoGroups bool) {
	t.Helper()
	for _, tbl := range []string{"tokens", "users", "abilities", "options"} {
		_, _ = raw.Exec("DROP TABLE IF EXISTS " + tbl)
	}
	for _, ddl := range schemaFor(d, withAutoGroups) {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("create table: %v\n%s", err, ddl)
		}
	}
	exec := func(q string, args ...any) {
		if _, err := raw.Exec(d.prepare(q), args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	gone := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exec(`INSERT INTO users (id, {group}, status, role, deleted_at) VALUES (?, ?, ?, ?, ?)`, 1, "default", 1, 100, nil) // 管理员
	exec(`INSERT INTO users (id, {group}, status, role, deleted_at) VALUES (?, ?, ?, ?, ?)`, 7, "default", 1, 1, nil)   // 普通用户

	type tok struct {
		id, user    int64
		key, group  string
		auto        string
		status      int
		expired     int64
		deletedAt   any
		description string
	}
	toks := []tok{
		{42, 7, "testtoken", "normal", "", 1, -1, nil, "普通用户的 key"},
		{99, 1, "testkey", "normal", "", 1, -1, nil, "管理员的 key，可当转述用"},
		{98, 1, "adminexpired", "normal", "", 1, 1000, nil, "管理员已过期的 key"},
		{50, 7, "expiredtoken", "normal", "", 1, 1000, nil, "已过期"},
		{51, 7, "disabledtoken", "normal", "", 2, -1, nil, "已停用"},
		{60, 7, "deletedtoken", "normal", "", 1, -1, gone, "已删除"},
		{61, 7, "usergrouptoken", "", "", 1, -1, nil, "令牌没写分组，跟用户分组"},
		{52, 7, "autotoken", "auto", "", 1, -1, nil, "auto 分组，用全站 AutoGroups"},
		{53, 7, "autotoken2", "auto", `["claude","normal"]`, 1, -1, nil, "auto 分组，用令牌自己的列表"},
	}
	for _, k := range toks {
		if withAutoGroups {
			exec(`INSERT INTO tokens (id, user_id, {key}, name, {group}, auto_groups, status, expired_time, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				k.id, k.user, k.key, k.description, k.group, k.auto, k.status, k.expired, k.deletedAt)
		} else {
			exec(`INSERT INTO tokens (id, user_id, {key}, name, {group}, status, expired_time, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				k.id, k.user, k.key, k.description, k.group, k.status, k.expired, k.deletedAt)
		}
	}
	abilities := []struct {
		group, model string
		channel      int
		enabled      bool
	}{
		{"normal", "deepseek-v4.1-flash", 1, true},
		{"normal", "deepseek-v4.1-flash", 2, true}, // 同一模型多个渠道，目录里要去重
		{"normal", "other-model", 1, true},
		{"normal", "disabled-model", 5, false},
		{"claude", "deepseek-v4.1-flash", 3, true},
		{"default", "misc-model", 4, true},
	}
	for _, ab := range abilities {
		exec(`INSERT INTO abilities ({group}, model, channel_id, enabled) VALUES (?, ?, ?, ?)`, ab.group, ab.model, ab.channel, ab.enabled)
	}
	exec(`INSERT INTO options ({key}, value) VALUES (?, ?)`, "AutoGroups", `["normal","claude"]`)
}

// newTestSQLite 建一个模仿 new-api 的 SQLite 库文件，返回路径。
func newTestSQLite(t *testing.T) string {
	t.Helper()
	return newTestSQLiteWith(t, true)
}

func newTestSQLiteWith(t *testing.T, withAutoGroups bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "one-api.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer raw.Close()
	seedNewAPI(t, raw, &newAPIDB{dialect: dialectSQLite}, withAutoGroups)
	return path
}

// checkDirectory 是三种库共用的断言。
func checkDirectory(t *testing.T, d *newAPIDB, withAutoGroups bool) {
	t.Helper()
	ctx := context.Background()

	id, err := d.identity(ctx, "testtoken")
	if err != nil || !id.Found || id.UserID != 7 || id.TokenID != 42 || id.TokenGroup != "normal" || id.UserGroup != "default" {
		t.Fatalf("identity(testtoken) = %+v, %v", id, err)
	}
	for _, k := range []string{"expiredtoken", "disabledtoken", "deletedtoken", "no-such-key"} {
		if id, err := d.identity(ctx, k); err != nil || id.Found {
			t.Fatalf("identity(%s) should not be found: %+v, %v", k, id, err)
		}
	}
	if id, _ := d.identity(ctx, "usergrouptoken"); id.TokenGroup != "default" {
		t.Fatalf("empty token group should fall back to user group: %+v", id)
	}
	if id, _ := d.identity(ctx, "autotoken"); id.TokenGroup != "auto" || !reflect.DeepEqual(id.AutoGroups, []string{"normal", "claude"}) {
		t.Fatalf("auto token should use the site-wide AutoGroups: %+v", id)
	}
	if withAutoGroups {
		if id, _ := d.identity(ctx, "autotoken2"); !reflect.DeepEqual(id.AutoGroups, []string{"claude", "normal"}) {
			t.Fatalf("auto token should use its own list first: %+v", id)
		}
	}

	cat, err := d.catalog(ctx)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if !cat.has("normal", "deepseek-v4.1-flash") || !cat.has("default", "misc-model") || !cat.has("claude", "deepseek-v4.1-flash") {
		t.Fatalf("catalog missing models: %+v", cat.Groups)
	}
	if cat.has("normal", "disabled-model") {
		t.Fatalf("disabled abilities must not be listed")
	}
	for _, g := range cat.Groups {
		if g.Name == "normal" && len(g.Models) != 2 {
			t.Fatalf("models should be de-duplicated: %v", g.Models)
		}
	}

	key, group, err := d.tokenKey(ctx, 99)
	if err != nil || key != "testkey" || group != "normal" {
		t.Fatalf("tokenKey(99) = %q %q %v", key, group, err)
	}
	for _, id := range []int64{42, 98, 12345} { // 普通用户的、管理员已过期的、不存在的
		if _, _, err := d.tokenKey(ctx, id); !errors.Is(err, errNoKey) {
			t.Fatalf("tokenKey(%d) should be refused, got %v", id, err)
		}
	}
}

func TestNewAPIDirectorySQLite(t *testing.T) {
	d, err := openNewAPIDB("", newTestSQLite(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if !d.autoGroups {
		t.Fatalf("auto_groups column should be detected")
	}
	checkDirectory(t, d, true)
}

func TestNewAPIDirectoryWithoutAutoGroupsColumn(t *testing.T) {
	d, err := openNewAPIDB("", newTestSQLiteWith(t, false))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if d.autoGroups {
		t.Fatalf("old schema has no auto_groups column")
	}
	checkDirectory(t, d, false)
}

func TestOpenNewAPIDBNeedsConfig(t *testing.T) {
	if _, err := openNewAPIDB("", ""); err == nil {
		t.Fatalf("missing database config should be an error")
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"Bearer sk-abcdef":    "abcdef",
		"bearer  sk-abcdef":   "abcdef",
		"sk-abcdef-12":        "abcdef", // new-api 允许 key 后面带「-渠道号」
		"abcdef":              "abcdef",
		"Bearer abcdef-extra": "abcdef",
		"":                    "",
	}
	for in, want := range cases {
		if got := normalizeKey(in); got != want {
			t.Fatalf("normalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewAPIDirectoryOnRealDatabases 在真实的 MySQL / PostgreSQL 上跑同一套检查。
func TestNewAPIDirectoryOnRealDatabases(t *testing.T) {
	cases := []struct {
		name, env, driver string
		dl                dialect
	}{
		{"mysql", "CCD_TEST_MYSQL_DSN", "mysql", dialectMySQL},
		{"postgres", "CCD_TEST_PG_DSN", "pgx", dialectPostgres},
	}
	for _, c := range cases {
		dsn := os.Getenv(c.env)
		if dsn == "" {
			t.Logf("skip %s: %s not set", c.name, c.env)
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			raw, err := sql.Open(c.driver, dsn)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer raw.Close()
			seedNewAPI(t, raw, &newAPIDB{dialect: c.dl}, true)
			defer func() {
				for _, tbl := range []string{"tokens", "users", "abilities", "options"} {
					_, _ = raw.Exec("DROP TABLE IF EXISTS " + tbl)
				}
			}()
			d, err := openNewAPIDB(dsn, "")
			if err != nil {
				t.Fatalf("openNewAPIDB: %v", err)
			}
			defer d.Close()
			if d.dialect != c.dl || !d.autoGroups {
				t.Fatalf("dialect=%v autoGroups=%v", d.dialect, d.autoGroups)
			}
			checkDirectory(t, d, true)
		})
	}
}
