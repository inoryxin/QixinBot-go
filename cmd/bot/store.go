package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// checkpointSQLite 把 WAL 合并进主库（备份前用）。
// 库不存在就跳过；被运行中的实例占用时 checkpoint 仍可执行（WAL 允许多进程）。
func checkpointSQLite(dbPath string) {
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		fmt.Fprintf(os.Stderr, "WAL checkpoint 失败（备份继续，但可能不是最新状态）: %v\n", err)
	}
}

// store 是插件私有 KV 存储。每个插件的 storage.json 放在它自己的数据目录里，
// 和插件写的其它文件待在一起，备份/删除都只涉及一个目录。
type store struct {
	dir string
	mu  sync.Mutex
}

func newStore(root string) *store {
	return &store{dir: root}
}

func (s *store) path(plugin string) string {
	return filepath.Join(s.dir, plugin, "data", "storage.json")
}

func (s *store) load(plugin string) map[string]any {
	data := map[string]any{}
	b, err := os.ReadFile(s.path(plugin))
	if err != nil {
		return data
	}
	if err := json.Unmarshal(b, &data); err != nil {
		// 损坏的存储不能当"空的"直接往下走：下一次 Set 会用只含
		// 新 key 的 map 把整个文件重写，插件攒的所有 KV 就全没了。
		// 先把坏文件留个底，再当空表处理（插件还能继续干活）
		bak := s.path(plugin) + ".corrupt-" + time.Now().Format("20060102-150405")
		if _, statErr := os.Stat(s.path(plugin)); statErr == nil {
			_ = os.Rename(s.path(plugin), bak)
			log.Printf("[store] storage.json 损坏，已备份到 %s，按空存储继续: %v", bak, err)
		}
	}
	return data
}

func (s *store) save(plugin string, data map[string]any) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	target := s.path(plugin)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// 原子写：先写临时文件再 rename，避免断电留下半截 JSON。
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	return os.Rename(tmp, target)
}

func (s *store) Get(plugin, key string) (any, bool) {
	start := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if waited := time.Since(start); waited > 100*time.Millisecond {
		log.Printf("[store] 等锁 %.0fms plugin=%s key=%s", float64(waited.Microseconds())/1000, plugin, key)
	}
	v, ok := s.load(plugin)[key]
	return v, ok
}

func (s *store) Set(plugin, key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.load(plugin)
	data[key] = value
	return s.save(plugin, data)
}

func (s *store) Delete(plugin, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.load(plugin)
	delete(data, key)
	return s.save(plugin, data)
}

func (s *store) List(plugin, prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.load(plugin)
	keys := make([]string, 0, len(data))
	for k := range data {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
