package main

// 只读 new-api 的数据库：按 key 认身份（用户、分组、令牌 id）、列出「分组 → 启用模型」、按令牌 id 取转述用的 key。
//
// 支持 new-api 自己支持的三种库，连接写法也与 new-api 一致：
//   - CCD_SQL_DSN 以 postgres 开头 → PostgreSQL（如 postgresql://user:pass@host:5432/new-api）
//   - CCD_SQL_DSN 其它非空写法 → MySQL（如 user:pass@tcp(host:3306)/new-api）
//   - CCD_SQL_DSN 为空 → SQLite，文件路径取 CCD_SQLITE_PATH（与 new-api 的 SQLITE_PATH 指向同一个文件）
//
// ccd 只执行 SELECT，从不写库。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type dialect int

const (
	dialectSQLite dialect = iota
	dialectMySQL
	dialectPostgres
)

// errNoKey 按 id 取 key 时没找到可用的令牌（不存在、已删除、已停用、已过期，或者不在管理员名下）。
var errNoKey = errors.New("这个 key 不存在、已删除、已停用、已过期，或者不在管理员名下")

type newAPIDB struct {
	db         *sql.DB
	dialect    dialect
	autoGroups bool // tokens 表有没有 auto_groups 列（老版本 new-api 没有）
}

// openNewAPIDB 按 new-api 的规则选库并连上，顺带探测 tokens 表有没有 auto_groups 列。
func openNewAPIDB(dsn, sqlitePath string) (*newAPIDB, error) {
	d := &newAPIDB{}
	var err error
	switch {
	case strings.HasPrefix(dsn, "postgres"):
		d.dialect = dialectPostgres
		d.db, err = sql.Open("pgx", dsn)
	case dsn != "":
		d.dialect = dialectMySQL
		d.db, err = sql.Open("mysql", dsn)
	case sqlitePath != "":
		// 只读打开。new-api 用 WAL 模式时，读者也要能写 -shm 文件，
		// 所以 ccd 要用能写 new-api 数据目录的身份运行（同一个系统用户，或 Docker 里挂同一个卷）。
		d.dialect = dialectSQLite
		d.db, err = sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro&_pragma=busy_timeout(5000)")
	default:
		return nil, errors.New("请设置 CCD_SQL_DSN（MySQL / PostgreSQL）或 CCD_SQLITE_PATH（SQLite），指向 new-api 的数据库")
	}
	if err != nil {
		return nil, err
	}
	d.db.SetMaxOpenConns(4)
	d.db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.db.PingContext(ctx); err != nil {
		d.db.Close()
		return nil, fmt.Errorf("连接 new-api 数据库失败：%w", err)
	}
	if rows, err := d.db.QueryContext(ctx, "SELECT auto_groups FROM tokens WHERE 1 = 0"); err == nil {
		rows.Close()
		d.autoGroups = true
	}
	return d, nil
}

func (d *newAPIDB) Close() error { return d.db.Close() }

// quote 给列名加引号：group、key 在 MySQL 里是保留字。
func (d *newAPIDB) quote(name string) string {
	if d.dialect == dialectMySQL {
		return "`" + name + "`"
	}
	return `"` + name + `"`
}

// prepare 把查询里的 {group}、{key} 换成加好引号的列名，PostgreSQL 再把 ? 换成 $1、$2……
func (d *newAPIDB) prepare(q string) string {
	q = strings.NewReplacer("{group}", d.quote("group"), "{key}", d.quote("key")).Replace(q)
	if d.dialect != dialectPostgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// identity 按 key（已去掉 sk- 前缀和渠道号后缀）查令牌和用户。令牌或用户已删除、已停用、令牌已过期都算查不到。
func (d *newAPIDB) identity(ctx context.Context, key string) (identity, error) {
	auto := "''"
	if d.autoGroups {
		auto = "t.auto_groups"
	}
	q := d.prepare(`SELECT t.id, t.user_id, t.{group}, ` + auto + `, t.status, t.expired_time, u.{group}, u.status
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.{key} = ? AND t.deleted_at IS NULL AND u.deleted_at IS NULL`)
	var (
		tokenID, userID, expired int64
		tokenStatus, userStatus  int
		tokenGroup, autoText     sql.NullString
		userGroup                sql.NullString
	)
	err := d.db.QueryRowContext(ctx, q, key).Scan(&tokenID, &userID, &tokenGroup, &autoText, &tokenStatus, &expired, &userGroup, &userStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return identity{}, nil
	}
	if err != nil {
		return identity{}, err
	}
	if tokenStatus != 1 || userStatus != 1 || (expired > 0 && expired < time.Now().Unix()) {
		return identity{}, nil
	}
	id := identity{Found: true, UserID: userID, TokenID: tokenID, TokenGroup: tokenGroup.String, UserGroup: userGroup.String}
	if id.TokenGroup == "" {
		id.TokenGroup = id.UserGroup
	}
	if id.TokenGroup == "auto" {
		groups := parseGroupList(autoText.String)
		if len(groups) == 0 {
			var opt sql.NullString
			err := d.db.QueryRowContext(ctx, d.prepare(`SELECT value FROM options WHERE {key} = ?`), "AutoGroups").Scan(&opt)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return identity{}, err
			}
			groups = parseGroupList(opt.String)
		}
		id.AutoGroups = groups
	}
	return id, nil
}

// parseGroupList 解析 JSON 数组形式的分组列表；空的或解析不了都当空。
func parseGroupList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

// catalog 列出每个分组下启用中的模型（abilities 表，一个渠道一行，这里去重）。
func (d *newAPIDB) catalog(ctx context.Context) (*catalog, error) {
	rows, err := d.db.QueryContext(ctx, d.prepare(`SELECT {group}, model FROM abilities WHERE enabled = TRUE ORDER BY {group}, model`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cat := &catalog{}
	seen := map[string]map[string]bool{}
	pos := map[string]int{}
	for rows.Next() {
		var group, model string
		if err := rows.Scan(&group, &model); err != nil {
			return nil, err
		}
		if seen[group] == nil {
			seen[group] = map[string]bool{}
			pos[group] = len(cat.Groups)
			cat.Groups = append(cat.Groups, catalogGroup{Name: group})
		}
		if !seen[group][model] {
			seen[group][model] = true
			g := &cat.Groups[pos[group]]
			g.Models = append(g.Models, model)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cat.build()
	return cat, nil
}

// tokenKey 按令牌 id 取转述用的 key 和它实际所在的分组：只认管理员（role>=100）名下、启用中、未过期的令牌。
func (d *newAPIDB) tokenKey(ctx context.Context, id int64) (key, group string, err error) {
	q := d.prepare(`SELECT t.{key}, t.{group}, u.{group}, t.expired_time
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.id = ? AND t.status = 1 AND t.deleted_at IS NULL
		AND u.deleted_at IS NULL AND u.status = 1 AND u.role >= 100`)
	var tokenGroup, userGroup sql.NullString
	var expired int64
	err = d.db.QueryRowContext(ctx, q, id).Scan(&key, &tokenGroup, &userGroup, &expired)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expired > 0 && expired < time.Now().Unix()) {
		return "", "", errNoKey
	}
	if err != nil {
		return "", "", err
	}
	group = tokenGroup.String
	if group == "" {
		group = userGroup.String
	}
	return strings.TrimSpace(key), group, nil
}
