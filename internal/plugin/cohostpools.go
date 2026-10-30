package plugin

// cohost 池登记（阶段 1）：只负责"池+插件归属"，不改变正在运行的 Host。
// 运行时按池启动/迁移在下一阶段接入。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	CohostDefaultPoolMax  = 12
	CohostStateQuarantine = "quarantine"
	CohostStateShared     = "shared"
)

type CohostPool struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MaxMembers int    `json:"max_members"`
	CreatedAt  int64  `json:"created_at"`
}

type CohostAssignment struct {
	PluginID  string `json:"plugin_id"`
	PoolID    string `json:"pool_id"`
	State     string `json:"state"`
	UpdatedAt int64  `json:"updated_at"`
	// CrashTimes 是该插件在共享池内的 worker 崩溃时间（Unix 秒），
	// 只保留最近 24h；逐出后仍保留，便于重启后继续按 3 次策略处理。
	CrashTimes []int64 `json:"crash_times,omitempty"`
}

type cohostPoolFile struct {
	Pools       []CohostPool                `json:"pools"`
	Assignments map[string]CohostAssignment `json:"assignments"`
}

type PoolStore struct {
	mu    sync.Mutex
	path  string
	pools []CohostPool
	byID  map[string]CohostAssignment
}

func NewPoolStore(path string) (*PoolStore, error) {
	s := &PoolStore{path: path, byID: map[string]CohostAssignment{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var f cohostPoolFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析 cohost-pools.json 失败: %w", err)
	}
	s.pools = f.Pools
	for _, a := range f.Assignments {
		if a.PluginID == "" {
			continue
		}
		if a.State == "" {
			a.State = CohostStateQuarantine
		}
		s.byID[a.PluginID] = a
	}
	return s, nil
}

func (s *PoolStore) List() ([]CohostPool, map[string]CohostAssignment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pools := append([]CohostPool(nil), s.pools...)
	ass := make(map[string]CohostAssignment, len(s.byID))
	for k, v := range s.byID {
		ass[k] = cloneAssignment(v)
	}
	return pools, ass
}

func (s *PoolStore) PoolOf(pluginID string) (CohostPool, CohostAssignment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[pluginID]
	if !ok || a.PoolID == "" {
		return CohostPool{}, cloneAssignment(a), false
	}
	for _, p := range s.pools {
		if p.ID == a.PoolID {
			return p, cloneAssignment(a), true
		}
	}
	return CohostPool{}, cloneAssignment(a), false
}

func (s *PoolStore) Create(name string, maxMembers int) (CohostPool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CohostPool{}, fmt.Errorf("池名不能为空")
	}
	if maxMembers <= 0 {
		maxMembers = CohostDefaultPoolMax
	}
	if maxMembers > 64 {
		return CohostPool{}, fmt.Errorf("池上限不能超过 64")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pools {
		if strings.EqualFold(p.Name, name) {
			return CohostPool{}, fmt.Errorf("池名已存在: %s", name)
		}
	}
	p := CohostPool{
		ID:         fmt.Sprintf("pool-%d", time.Now().UnixNano()),
		Name:       name,
		MaxMembers: maxMembers,
		CreatedAt:  time.Now().Unix(),
	}
	s.pools = append(s.pools, p)
	if err := s.saveLocked(); err != nil {
		return CohostPool{}, err
	}
	return p, nil
}

func (s *PoolStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.byID {
		if a.PoolID == id {
			return fmt.Errorf("池里还有插件，先移出再删除")
		}
	}
	out := s.pools[:0]
	found := false
	for _, p := range s.pools {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return fmt.Errorf("池不存在")
	}
	s.pools = out
	return s.saveLocked()
}

// Assign 把插件移入池；poolID 为空 = quarantine（不迁移运行中的进程，阶段 1 只登记）。
func (s *PoolStore) Assign(pluginID, poolID string) error {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return fmt.Errorf("plugin_id 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		delete(s.byID, pluginID)
		return s.saveLocked()
	}
	var pool *CohostPool
	for i := range s.pools {
		if s.pools[i].ID == poolID {
			pool = &s.pools[i]
			break
		}
	}
	if pool == nil {
		return fmt.Errorf("池不存在")
	}
	count := 0
	for id, a := range s.byID {
		if a.PoolID == poolID && a.State == CohostStateShared && id != pluginID {
			count++
		}
	}
	if pool.MaxMembers > 0 && count >= pool.MaxMembers {
		return fmt.Errorf("池 %s 已满（上限 %d）", pool.Name, pool.MaxMembers)
	}
	s.byID[pluginID] = CohostAssignment{
		PluginID: pluginID, PoolID: poolID, State: CohostStateShared, UpdatedAt: time.Now().Unix(),
	}
	return s.saveLocked()
}

// UpdatePool 修改池名/上限；缩容时不能低于当前共享成员数。
func (s *PoolStore) UpdatePool(id, name string, maxMembers int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("池名不能为空")
	}
	if maxMembers <= 0 {
		maxMembers = CohostDefaultPoolMax
	}
	if maxMembers > 64 {
		return fmt.Errorf("池上限不能超过 64")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var pool *CohostPool
	for i := range s.pools {
		if s.pools[i].ID == id {
			pool = &s.pools[i]
			break
		}
	}
	if pool == nil {
		return fmt.Errorf("池不存在")
	}
	for _, p := range s.pools {
		if p.ID != id && strings.EqualFold(p.Name, name) {
			return fmt.Errorf("池名已存在: %s", name)
		}
	}
	n := 0
	for _, a := range s.byID {
		if a.PoolID == id && a.State == CohostStateShared {
			n++
		}
	}
	if n > maxMembers {
		return fmt.Errorf("成员数 %d 超过上限 %d", n, maxMembers)
	}
	pool.Name = name
	pool.MaxMembers = maxMembers
	return s.saveLocked()
}

func cloneAssignment(a CohostAssignment) CohostAssignment {
	a.CrashTimes = append([]int64(nil), a.CrashTimes...)
	return a
}

// RecordCrash 记录一次共享 worker 崩溃，返回 24h 内的次数。
func (s *PoolStore) RecordCrash(pluginID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[pluginID]
	if !ok {
		return 0, nil
	}
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	times := a.CrashTimes[:0]
	for _, t := range a.CrashTimes {
		if t > cutoff {
			times = append(times, t)
		}
	}
	times = append(times, time.Now().Unix())
	a.CrashTimes = times
	a.UpdatedAt = time.Now().Unix()
	s.byID[pluginID] = a
	return len(times), s.saveLocked()
}

// ResetCrashes 清空人工恢复插件的 24h 崩溃计数。
func (s *PoolStore) ResetCrashes(pluginID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[pluginID]
	if !ok {
		return nil
	}
	a.CrashTimes = nil
	s.byID[pluginID] = a
	return s.saveLocked()
}

// Evict 把插件标记为已逐出（保留原池名便于 UI 显示），运行时按 quarantine 处理。
func (s *PoolStore) Evict(pluginID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[pluginID]
	if !ok {
		return nil
	}
	a.State = "evicted"
	a.UpdatedAt = time.Now().Unix()
	s.byID[pluginID] = a
	return s.saveLocked()
}

func (s *PoolStore) removePlugin(pluginID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[pluginID]; !ok {
		return nil
	}
	delete(s.byID, pluginID)
	return s.saveLocked()
}

func (s *PoolStore) saveLocked() error {
	f := cohostPoolFile{Pools: s.pools, Assignments: s.byID}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
