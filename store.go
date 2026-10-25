package main

// 「客户端看到的摘要 → 上游原版思维链」的持久化存储。
//
// 目录结构：<root>/<user_id>/<key>，一个 key 一个文件，文件内容就是原版思维链的原始字节。
// 按用户分目录既做到严格隔离，又让“这个用户有没有收到过转述”变成一次目录判断（零解析透传的依据）。
// key 由调用方算好（含前缀哈希或签名哈希），这里只管落盘、读取、过期和容量淘汰。

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// maxRecordBytes 单条记录上限：正常思维链远小于此值，超了就不存（代价只是这一轮换不回原版）。
const maxRecordBytes = 512 << 10

type diskStore struct {
	root     string
	ttl      time.Duration
	maxBytes int64
	maxRecs  int

	mu    sync.Mutex
	cur   int64
	count int
	users map[int64]bool
	last  time.Time
}

func newDiskStore(root string, ttl time.Duration, maxBytes int64) (*diskStore, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	s := &diskStore{
		root:     root,
		ttl:      ttl,
		maxBytes: maxBytes,
		maxRecs:  400000,
		users:    map[int64]bool{},
	}
	s.scan()
	return s, nil
}

// scan 启动时重建索引：只统计文件大小和目录，不读文件内容。
func (s *diskStore) scan() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = map[int64]bool{}
	s.cur, s.count = 0, 0
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		uid, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		dir := filepath.Join(s.root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil || len(files) == 0 {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			s.cur += info.Size()
			s.count++
		}
		if s.count > 0 {
			s.users[uid] = true
		}
	}
	s.last = time.Now()
	log.Printf("store: %d records, %d users, %.1f MiB under %s", s.count, len(s.users), float64(s.cur)/(1<<20), s.root)
}

// Has 判断该用户是否曾经收到过转述（没有就完全跳过请求体解析）。
func (s *diskStore) Has(uid int64) bool {
	if s == nil || uid <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.users[uid]
}

func (s *diskStore) path(uid int64, key string) string {
	return filepath.Join(s.root, strconv.FormatInt(uid, 10), key)
}

// Put 写一条记录。key 需已包含用户维度（同目录）；raw 为空或过大则忽略。
func (s *diskStore) Put(uid int64, key string, raw string) {
	if s == nil || uid <= 0 || key == "" || raw == "" || len(raw) > maxRecordBytes {
		return
	}
	dir := filepath.Join(s.root, strconv.FormatInt(uid, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("store: mkdir failed: %v", err)
		return
	}
	p := filepath.Join(dir, key)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw), 0o600); err != nil {
		log.Printf("store: write failed: %v", err)
		return
	}
	old := int64(0)
	if fi, err := os.Stat(p); err == nil {
		old = fi.Size()
	} else {
		s.mu.Lock()
		s.count++
		s.users[uid] = true
		s.mu.Unlock()
	}
	if err := os.Rename(tmp, p); err != nil {
		log.Printf("store: rename failed: %v", err)
		_ = os.Remove(tmp)
		return
	}
	s.mu.Lock()
	s.cur += int64(len(raw)) - old
	s.mu.Unlock()
}

// Get 取回原版；不存在或已过期返回 false。
func (s *diskStore) Get(uid int64, key string) (string, bool) {
	if s == nil || uid <= 0 || key == "" {
		return "", false
	}
	p := s.path(uid, key)
	fi, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	if s.ttl > 0 && time.Since(fi.ModTime()) > s.ttl {
		s.remove(uid, p, fi.Size())
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (s *diskStore) remove(uid int64, p string, size int64) {
	if err := os.Remove(p); err != nil {
		return
	}
	s.mu.Lock()
	s.cur -= size
	s.count--
	if s.count < 0 {
		s.count = 0
	}
	s.mu.Unlock()
}

// Cleanup 清理过期记录；超过容量上限时按修改时间从旧到新淘汰。
func (s *diskStore) Cleanup() {
	if s == nil {
		return
	}
	type rec struct {
		uid  int64
		path string
		size int64
		at   time.Time
	}
	var recs []rec
	var total int64
	_ = filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".tmp") {
			_ = os.Remove(p)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		uid, err := strconv.ParseInt(filepath.Base(filepath.Dir(p)), 10, 64)
		if err != nil {
			return nil
		}
		recs = append(recs, rec{uid: uid, path: p, size: info.Size(), at: info.ModTime()})
		total += info.Size()
		return nil
	})
	now := time.Now()
	live := recs[:0]
	for _, r := range recs {
		if s.ttl > 0 && now.Sub(r.at) > s.ttl {
			_ = os.Remove(r.path)
			total -= r.size
			continue
		}
		live = append(live, r)
	}
	if s.maxBytes > 0 && (total > s.maxBytes || len(live) > s.maxRecs) {
		sort.Slice(live, func(i, j int) bool { return live[i].at.Before(live[j].at) })
		for _, r := range live {
			if total <= s.maxBytes && len(live) <= s.maxRecs {
				break
			}
			_ = os.Remove(r.path)
			total -= r.size
			live = live[1:]
		}
	}
	users := map[int64]bool{}
	for _, r := range live {
		users[r.uid] = true
	}
	s.mu.Lock()
	s.cur, s.count, s.users, s.last = total, len(live), users, now
	s.mu.Unlock()
}

// hashKey 生成存储用的键：同一段内容在不同用户/不同场景下互不干扰。
func hashKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeText 去掉所有空白：客户端回传摘要时换行/空格常有细微差别，不能因此换不回原版。
func normalizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
