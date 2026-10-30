// Package storage 是框架的持久化抽象。
//
// 两个后端：
//   - json   ：一个文件存全部，简单、零依赖（默认）
//   - sqlite ：装了大数量插件后事务和并发更稳，首次启动自动从 JSON 迁移
//
// 选哪个由 config.toml 的 [storage] driver 决定。
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，不需要 CGO
)

// Backend 是键值存储的统一接口。
type Backend interface {
	// Get 读一个 key；不存在返回 (nil, false, nil)
	Get(bucket, key string) ([]byte, bool, error)
	// Set 写一个 key
	Set(bucket, key string, value []byte) error
	// Delete 删一个 key
	Delete(bucket, key string) error
	// List 列出一个 bucket 的所有 key
	List(bucket string) (map[string][]byte, error)
	// Close 收尾
	Close() error
}

// ---------------------------------------------------------------- SQLite

// SQLite 是 SQLite 后端。
type SQLite struct {
	mu  sync.RWMutex
	db  *sql.DB
	dsn string
}

// OpenSQLite 打开框架 KV 库（会建 kv 表）。
func OpenSQLite(path string) (*SQLite, error) {
	return openSQLite(path, true)
}

// OpenPluginDB 打开插件业务库。
//
// **刻意不建 kv 表**——它是给插件自由建表用的，
// 和框架内部的 KV 是两个物理文件，插件 SQL 读不到框架数据。
func OpenPluginDB(path string) (*SQLite, error) {
	return openSQLite(path, false)
}

func openSQLite(path string, withKV bool) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// _busy_timeout：写冲突时等一会儿而不是立刻报错
	// _journal_mode=WAL：读写不互相阻塞
	dsn := path + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db.SetMaxOpenConns(1) // 单写者，避免锁竞争
	if withKV {
		if _, err := db.Exec(`
			CREATE TABLE IF NOT EXISTS kv (
				bucket     TEXT NOT NULL,
				key        TEXT NOT NULL,
				value      BLOB NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (bucket, key)
			);
			CREATE INDEX IF NOT EXISTS idx_kv_bucket ON kv(bucket);
		`); err != nil {
			db.Close()
			return nil, fmt.Errorf("建表失败: %w", err)
		}
	}
	return &SQLite{db: db, dsn: path}, nil
}

func (s *SQLite) Get(bucket, key string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v []byte
	err := s.db.QueryRow(`SELECT value FROM kv WHERE bucket=? AND key=?`, bucket, key).Scan(&v)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (s *SQLite) Set(bucket, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO kv (bucket, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(bucket, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
	`, bucket, key, value, time.Now().Unix())
	return err
}

func (s *SQLite) Delete(bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM kv WHERE bucket=? AND key=?`, bucket, key)
	return err
}

func (s *SQLite) List(bucket string) (map[string][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT key, value FROM kv WHERE bucket=?`, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]byte)
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }

// DB 暴露底层连接，让框架把 db.sqlite 能力直接挂上去（不经过插件）。
func (s *SQLite) DB() *sql.DB { return s.db }

// Stats 返回行数和文件大小，给 WebUI 展示。
func (s *SQLite) Stats() (rows int, size int64) {
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM kv`).Scan(&rows)
	if st, err := os.Stat(s.dsn); err == nil {
		size = st.Size()
	}
	return
}

// ---------------------------------------------------------------- JSON

// JSON 是"一个文件存全部"的后端，兼容老版本。
type JSON struct {
	mu   sync.RWMutex
	path string
	data map[string]map[string]json.RawMessage // bucket -> key -> value
}

// OpenJSON 读入已有文件。
func OpenJSON(path string) (*JSON, error) {
	j := &JSON{path: path, data: make(map[string]map[string]json.RawMessage)}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return j, nil
		}
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &j.data); err != nil {
			return nil, fmt.Errorf("JSON 存储损坏: %w", err)
		}
	}
	return j, nil
}

func (j *JSON) Get(bucket, key string) ([]byte, bool, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	b, ok := j.data[bucket]
	if !ok {
		return nil, false, nil
	}
	v, ok := b[key]
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

func (j *JSON) Set(bucket, key string, value []byte) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.data[bucket] == nil {
		j.data[bucket] = make(map[string]json.RawMessage)
	}
	j.data[bucket][key] = json.RawMessage(value)
	return j.flushLocked()
}

func (j *JSON) Delete(bucket, key string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if b, ok := j.data[bucket]; ok {
		delete(b, key)
	}
	return j.flushLocked()
}

func (j *JSON) List(bucket string) (map[string][]byte, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make(map[string][]byte)
	for k, v := range j.data[bucket] {
		out[k] = v
	}
	return out, nil
}

func (j *JSON) Close() error { return nil }

// flushLocked 原子写盘（先写 tmp 再 rename）。
func (j *JSON) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	return os.Rename(tmp, j.path)
}

// ---------------------------------------------------------------- 迁移

// Migrate 把 JSON 后端的数据搬进 SQLite。返回搬了多少条。
// 只在目标库为空时搬，避免覆盖新数据。
func Migrate(from Backend, to Backend, buckets []string) (int, error) {
	moved := 0
	for _, bucket := range buckets {
		existing, err := to.List(bucket)
		if err != nil {
			return moved, err
		}
		if len(existing) > 0 {
			continue // 这个 bucket 已经有数据了，跳过
		}
		items, err := from.List(bucket)
		if err != nil {
			return moved, err
		}
		for k, v := range items {
			if err := to.Set(bucket, k, v); err != nil {
				return moved, fmt.Errorf("迁移 %s/%s 失败: %w", bucket, k, err)
			}
			moved++
		}
	}
	return moved, nil
}
