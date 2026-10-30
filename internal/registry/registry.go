package registry

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/xin/onebot/internal/perm"
)

// Kind 是功能类型。help 看 command，AI 看 tool，权限看全部。
type Kind string

const (
	KindCommand Kind = "command"
	KindTool    Kind = "tool"
	KindEvent   Kind = "event"
)

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)+$`)

// ValidID 判断 feature id 是否符合 "plugin.verb" 形式。
func ValidID(id string) bool { return idRe.MatchString(id) }

// Feature 是框架里的一等公民：权限、help、AI 工具列表都是它的投影。
type Feature struct {
	ID          string
	Plugin      string
	Kind        Kind
	Label       string
	Usage       string
	Description string
	Hidden      bool
	Policy      perm.Policy

	// RequirePrefix / RequireMention 是命令触发的门禁，WebUI 可覆盖
	RequirePrefix  bool
	RequireMention bool
}

// Registry 保存全部 Feature，按注册顺序返回。
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]*Feature
	order []string
}

// New 创建空注册表。
func New() *Registry {
	return &Registry{byID: make(map[string]*Feature)}
}

// Add 注册一个 Feature；ID 冲突直接报错（不覆盖）。
func (r *Registry) Add(f *Feature) error {
	if f == nil {
		return fmt.Errorf("feature 不能为空")
	}
	if !idRe.MatchString(f.ID) {
		return fmt.Errorf("feature id %q 不合法，必须形如 plugin.verb", f.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[f.ID]; exists {
		return fmt.Errorf("feature id 冲突: %s（已被占用）", f.ID)
	}
	r.byID[f.ID] = f
	r.order = append(r.order, f.ID)
	return nil
}

// RemoveByPlugin 回收某个插件的全部 Feature，返回删除数量。
func (r *Registry) RemoveByPlugin(plugin string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := 0
	kept := r.order[:0]
	for _, id := range r.order {
		if f, ok := r.byID[id]; ok && f.Plugin == plugin {
			delete(r.byID, id)
			removed++
			continue
		}
		kept = append(kept, id)
	}
	r.order = kept
	return removed
}

// All 返回全部 Feature 的副本（按注册顺序）。
func (r *Registry) All() []*Feature {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Feature, 0, len(r.order))
	for _, id := range r.order {
		if f, ok := r.byID[id]; ok {
			out = append(out, f)
		}
	}
	return out
}

// Get 按 ID 取。
func (r *Registry) Get(id string) *Feature {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
}

// Plugins 返回当前有 Feature 的插件名列表（去重、保序）。
func (r *Registry) Plugins() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, id := range r.order {
		f := r.byID[id]
		if f == nil || seen[f.Plugin] {
			continue
		}
		seen[f.Plugin] = true
		out = append(out, f.Plugin)
	}
	return out
}
