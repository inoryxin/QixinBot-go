// Package shared 是框架托管的跨插件数据。
// 插件在 manifest 里用 [[shared]] 声明自己能提供哪些共享值，
// 其他插件通过 host.shared_* 读写。key 自动加 "<插件id>." 前缀，不会撞。
package shared

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/xin/onebot/internal/storage"
)

// Entry 是给 WebUI / 插件看的一条共享数据。
type Entry struct {
	Key         string `json:"key"`
	Owner       string `json:"owner"`
	Description string `json:"description,omitempty"`
	Value       any    `json:"value"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	meta map[string]Entry
	// backend 非 nil 时走统一存储（JSON 或 SQLite），path 只用于首次迁移
	backend storage.Backend
}

// bucket 是共享数据在统一存储里的分组名。
const bucket = "shared"

// New 读取磁盘上的共享数据；不存在就是空的。
func New(path string) *Store {
	s := &Store{path: path, meta: make(map[string]Entry)}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var list []Entry
	if json.Unmarshal(raw, &list) != nil {
		// 损坏文件先备份再重建：否则下一次 Register/Set 会把
		// 之前共享的所有值无声覆盖掉（与 taskStore 同款处理）
		bak := path + ".corrupt-" + time.Now().Format("20060102-150405")
		if copyErr := os.Rename(path, bak); copyErr == nil {
			slog.Warn("共享数据文件损坏，已备份并按空数据继续", "backup", bak, "err", err)
		}
		return s
	}
	for _, e := range list {
		s.meta[e.Key] = e
	}
	return s
}

// NewWithBackend 从统一存储加载共享数据。
// 如果 backend 里还没有数据、但老的 JSON 文件在，会顺手迁进来。
func NewWithBackend(b storage.Backend, legacyPath string) *Store {
	s := &Store{backend: b, path: legacyPath, meta: make(map[string]Entry)}

	items, err := b.List(bucket)
	if err != nil {
		return s
	}

	for k, raw := range items {
		var e Entry
		if json.Unmarshal(raw, &e) == nil {
			e.Key = k
			s.meta[k] = e
		}
	}

	// 老 JSON 里独有的 key 合并进来；后端已有的值优先，避免旧文件覆盖新数据。
	if legacyPath != "" {
		if old := New(legacyPath); len(old.meta) > 0 {
			for k, e := range old.meta {
				if _, exists := s.meta[k]; exists {
					continue
				}
				raw, _ := json.Marshal(e)
				_ = b.Set(bucket, k, raw)
				s.meta[k] = e
			}
		}
	}
	return s
}

// Register 由宿主在插件加载时调用：登记 key、补默认值。
// 已存在且属于同一个 owner 时不覆盖当前值，只更新描述。
func (s *Store) Register(owner, key, description string, def any) {
	full := owner + "." + key
	s.mu.Lock()
	defer s.mu.Unlock()

	if cur, ok := s.meta[full]; ok {
		if cur.Owner == owner {
			cur.Description = description
			cur.Value = keepIfSet(cur.Value, def)
			s.meta[full] = cur
			s.saveLocked()
		}
		return
	}
	s.meta[full] = Entry{Key: full, Owner: owner, Description: description, Value: def}
	s.saveLocked()
}

func keepIfSet(cur, def any) any {
	if cur != nil {
		return cur
	}
	return def
}

// Unregister 插件卸载时清掉它的条目。
func (s *Store) Unregister(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k, e := range s.meta {
		if e.Owner == owner {
			delete(s.meta, k)
			changed = true
			if s.backend != nil {
				_ = s.backend.Delete(bucket, k)
			}
		}
	}
	if changed {
		s.saveLocked()
	}
}

// Get 按完整 key 读。
func (s *Store) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.meta[key]
	if !ok {
		return nil, false
	}
	return e.Value, true
}

// Set 写值。只有声明过这个 key 的插件才能改。
func (s *Store) Set(owner, key string, value any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.meta[key]
	if !ok || e.Owner != owner {
		return false
	}
	e.Value = value
	s.meta[key] = e
	s.saveLocked()
	return true
}

// List 返回全部条目，按 key 排序。
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.meta))
	for _, e := range s.meta {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (s *Store) saveLocked() {
	if s.backend != nil {
		for k, e := range s.meta {
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			_ = s.backend.Set(bucket, k, raw)
		}
		return
	}
	if s.path == "" {
		return
	}
	// 不能用 List()：它会再拿 RLock，而这里已经持有写锁，RWMutex 不可重入
	out := make([]Entry, 0, len(s.meta))
	for _, e := range s.meta {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
