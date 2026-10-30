// Package override 保存 WebUI 上对插件行为的运行时覆盖：
// 命令权限白名单、以及 /help 的显示开关。
//
// 它和 plugin.toml 的关系：清单是作者给的默认值，这里是管理员在界面上改的，
// 优先级更高。存一个 JSON，方便备份和手改。
package override

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Package-level logger，避免每个方法都带 logger 参数；
// New 时会覆盖成宿主传入的。
var logger = slog.Default()

// Policy 是命令级权限覆盖。空字段表示"沿用清单里的声明"。
type Policy struct {
	AllowUsers  []int64 `json:"allow_users,omitempty"`
	AllowGroups []int64 `json:"allow_groups,omitempty"`
	RequireRole string  `json:"require_role,omitempty"`

	// nil 表示沿用清单里的默认值
	RequirePrefix  *bool `json:"require_prefix,omitempty"`
	RequireMention *bool `json:"require_mention,omitempty"`
}

// HelpRule 控制显示：key 可以是插件 id（控制整块）或 feature id（控制单条）。
type HelpRule struct {
	Show   *bool `json:"show,omitempty"`   // 插件级：整块是否出现在 /help
	Hidden *bool `json:"hidden,omitempty"` // 命令级：是否隐藏
}

// Data 是磁盘格式。
type Data struct {
	Policies map[string]Policy   `json:"policies"`
	Help     map[string]HelpRule `json:"help"`
	// Priority 是管理员在界面上改的分发优先级（大的先）。
	// 没配就用清单里的值。
	Priority map[string]int `json:"priority,omitempty"`
}

// Store 是并发安全的覆盖配置。
type Store struct {
	mu   sync.RWMutex
	path string
	data Data
}

// New 读取覆盖文件；不存在就返回空配置。
func New(path string) *Store {
	s := &Store{path: path, data: Data{
		Policies: make(map[string]Policy),
		Help:     make(map[string]HelpRule),
		Priority: make(map[string]int),
	}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		// 覆盖文件坏了不能装没事：管理员改的权限/优先级会"全部悄悄归零"
		logger.Error("覆盖文件损坏，已忽略（管理员设置的权限与优先级将回到清单默认值）",
			"path", path, "err", err)
		return s
	}
	if d.Policies != nil {
		s.data.Policies = d.Policies
	}
	if d.Help != nil {
		s.data.Help = d.Help
	}
	// 之前这里写反了：判断的是 s.data.Priority（永远非 nil），
	// 磁盘上的优先级从来没被加载过，重启即丢失
	if d.Priority != nil {
		s.data.Priority = d.Priority
	}
	return s
}

// Policy 返回某个 feature 的覆盖策略。第二个返回值表示是否存在覆盖。
func (s *Store) Policy(featureID string) (Policy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.data.Policies[featureID]
	return p, ok
}

// SetPolicy 写入/清除某个 feature 的覆盖。传空 Policy 等于清除。
func (s *Store) SetPolicy(featureID string, p Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p.AllowUsers) == 0 && len(p.AllowGroups) == 0 && p.RequireRole == "" &&
		p.RequirePrefix == nil && p.RequireMention == nil {
		delete(s.data.Policies, featureID)
	} else {
		s.data.Policies[featureID] = p
	}
	s.saveLocked()
}

// PluginVisible 判断某个插件的整块是否出现在 /help，默认可见。
func (s *Store) PluginVisible(plugin string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.data.Help[plugin]; ok && r.Show != nil {
		return *r.Show
	}
	return true
}

// FeatureHidden 返回命令的隐藏覆盖；nil 表示沿用清单里的 hidden。
func (s *Store) FeatureHidden(featureID string) *bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.data.Help[featureID]; ok && r.Hidden != nil {
		return r.Hidden
	}
	return nil
}

// SetPluginVisible 设置插件级显示开关。
func (s *Store) SetPluginVisible(plugin string, visible bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.data.Help[plugin]
	r.Show = &visible
	s.data.Help[plugin] = r
	s.saveLocked()
}

// SetFeatureHidden 设置命令级隐藏开关；nil 表示恢复清单默认。
func (s *Store) SetFeatureHidden(featureID string, hidden *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.data.Help[featureID]
	r.Hidden = hidden
	if r.Show == nil && r.Hidden == nil {
		delete(s.data.Help, featureID)
	} else {
		s.data.Help[featureID] = r
	}
	s.saveLocked()
}

// Priority 返回管理员设置的优先级；没有就 ok=false。
func (s *Store) Priority(plugin string) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data.Priority[plugin]
	return v, ok
}

// SetPriority 设置插件分发优先级（数字大的先处理）。
func (s *Store) SetPriority(plugin string, value int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Priority == nil {
		s.data.Priority = make(map[string]int)
	}
	s.data.Priority[plugin] = value
	s.saveLocked()
}

// Snapshot 返回一份副本，供 WebUI 展示。
func (s *Store) Snapshot() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	policies := make(map[string]Policy, len(s.data.Policies))
	for k, v := range s.data.Policies {
		policies[k] = v
	}
	help := make(map[string]HelpRule, len(s.data.Help))
	for k, v := range s.data.Help {
		help[k] = v
	}
	prio := make(map[string]int, len(s.data.Priority))
	for k, v := range s.data.Priority {
		prio[k] = v
	}
	return Data{Policies: policies, Help: help, Priority: prio}
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		logger.Error("覆盖文件序列化失败", "path", s.path, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		logger.Error("覆盖文件目录创建失败", "path", s.path, "err", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		logger.Error("覆盖文件写入失败", "path", s.path, "err", err)
		return
	}
	// 先 fsync 再 rename：否则掉电时 rename 先落盘、数据还在页缓存里，
	// 重启后文件是空的——"原子写"就白做了
	if f, ferr := os.Open(tmp); ferr == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, s.path); err != nil {
		logger.Error("覆盖文件替换失败", "path", s.path, "err", err)
	}
}
