package plugin

// 工具（tools）注册与调用、插件访问权限判定。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xin/onebot/internal/perm"
	"github.com/xin/onebot/internal/session"
)

// allow 判定"这个用户能不能用这个插件"。
// 优先级：插件级 [access] → 命中命令的命令级覆盖 → WebUI 覆盖（按插件）。
func (m *Manager) allow(man *Manifest, s *session.Session) bool {
	ok, _ := m.allowWithReason(man, s)
	return ok
}

// allowWithReason 在 allow 的基础上返回拒绝原因，供排查用。

// allowWithReason 在 allow 的基础上返回拒绝原因，供排查用。
func (m *Manager) allowWithReason(man *Manifest, s *session.Session) (bool, string) {
	// 1. 插件级起点
	p := perm.Policy{
		AllowUsers:  man.Access.AllowUsers,
		AllowGroups: man.Access.AllowGroups,
		RequireRole: man.Access.RequireRole,
	}
	requirePrefix := boolOr(man.Access.RequirePrefix, true)
	requireMention := boolOr(man.Access.RequireMention, false)

	// 2. 命中的命令如果显式写了权限，覆盖插件级（高级用法）
	var best *CommandSpec
	bestLen := -1
	for i := range man.Commands {
		if n, ok := matchCommandLen(man.Commands[i], s.Text); ok && n > bestLen {
			best = &man.Commands[i]
			bestLen = n
		}
	}
	if best != nil && hasCommandOverride(best) {
		if len(best.AllowUsers) > 0 {
			p.AllowUsers = best.AllowUsers
		}
		if len(best.AllowGroups) > 0 {
			p.AllowGroups = best.AllowGroups
		}
		if best.RequireRole != "" {
			p.RequireRole = best.RequireRole
		}
		if best.RequirePrefix != nil {
			requirePrefix = *best.RequirePrefix
		}
		if best.RequireMention != nil {
			requireMention = *best.RequireMention
		}
	}

	// 3. WebUI 上按插件改的配置优先级最高
	if m.Overrides != nil {
		if ov, ok := m.Overrides.Policy(man.ID); ok {
			p = perm.Policy{
				AllowUsers:  ov.AllowUsers,
				AllowGroups: ov.AllowGroups,
				RequireRole: ov.RequireRole,
			}
			if ov.RequirePrefix != nil {
				requirePrefix = *ov.RequirePrefix
			}
			if ov.RequireMention != nil {
				requireMention = *ov.RequireMention
			}
		}
	}

	if requirePrefix && !s.HasPrefix {
		return false, "需要命令前缀"
	}
	if requireMention && s.IsGroup() && !s.Mentioned {
		return false, "群里需要 @ 机器人"
	}
	if !m.Checker.Check(p, s) {
		return false, "权限不足"
	}
	return true, ""
}

// hasCommandOverride 判断某条命令是否显式声明了权限（用于覆盖插件级）。

// hasCommandOverride 判断某条命令是否显式声明了权限（用于覆盖插件级）。
func hasCommandOverride(c *CommandSpec) bool {
	return len(c.AllowUsers) > 0 || len(c.AllowGroups) > 0 || c.RequireRole != "" ||
		c.RequirePrefix != nil || c.RequireMention != nil
}

// fixedPrefix 从 usage 里取出不含参数的固定部分。
// "/selftest echo <文本>" → "selftest echo"；"/天气 <城市>" → "天气"

// fixedPrefix 从 usage 里取出不含参数的固定部分。
// "/selftest echo <文本>" → "selftest echo"；"/天气 <城市>" → "天气"
func fixedPrefix(usage string) string {
	tokens := strings.Fields(usage)
	var fixed []string
	for _, t := range tokens {
		if strings.HasPrefix(t, "<") || strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
			break
		}
		fixed = append(fixed, t)
	}
	if len(fixed) == 0 {
		return ""
	}
	return strings.TrimPrefix(strings.Join(fixed, " "), "/")
}

// matchCommandLen 返回命中的固定前缀长度，用于在多条命令里选最长匹配。

// matchCommandLen 返回命中的固定前缀长度，用于在多条命令里选最长匹配。
func matchCommandLen(c CommandSpec, text string) (int, bool) {
	if text == "" {
		return 0, false
	}
	candidates := append([]string{fixedPrefix(c.Usage)}, c.Aliases...)
	for _, cand := range candidates {
		cand = strings.TrimPrefix(cand, "/")
		if cand == "" {
			continue
		}
		if text == cand || strings.HasPrefix(text, cand+" ") {
			return len(cand), true
		}
		// 中文没有空格分隔，允许命令名后面直接跟参数
		if !strings.Contains(cand, " ") && strings.HasPrefix(text, cand) {
			return len(cand), true
		}
	}
	return 0, false
}

// Hosts 返回已就绪的插件句柄（按加载顺序）。

// ToolInfo 是暴露给 WebUI / AI 的工具描述。
type ToolInfo struct {
	Name        string `json:"name"`
	Plugin      string `json:"plugin"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

// DispatchEvent 把非消息事件广播给所有插件。单个插件失败不影响其它插件。
// subscribesEvent 判断插件是否订阅了这条事件。

// ListTools 汇总所有插件的工具声明。
func (m *Manager) ListTools() []ToolInfo {
	m.mu.RLock()
	hosts := make([]*Host, 0, len(m.order))
	for _, id := range m.order {
		if h, ok := m.hosts[id]; ok {
			hosts = append(hosts, h)
		}
	}
	m.mu.RUnlock()

	var out []ToolInfo
	for _, h := range hosts {
		for _, t := range h.Manifest.Tools {
			out = append(out, ToolInfo{
				Name:        t.Name,
				Plugin:      h.Manifest.ID,
				Description: t.Description,
				Parameters:  t.Parameters,
			})
		}
	}
	return out
}

// ListToolsFor 返回某个会话能用的工具。session 为 nil 时返回全部（管理员视角）。

// ListToolsFor 返回某个会话能用的工具。session 为 nil 时返回全部（管理员视角）。
func (m *Manager) ListToolsFor(s *session.Session) []ToolInfo {
	all := m.ListTools()
	if s == nil {
		return all
	}

	// 工具可见性 ≠ 命令触发条件：用户能不能用这个工具，只看权限名单，
	// 跟"命令要不要带前缀/要不要 @"无关。这里复制一份把门禁绕开。
	probe := *s
	probe.HasPrefix = true
	probe.Mentioned = true

	out := make([]ToolInfo, 0, len(all))
	for _, t := range all {
		if m.CheckAccess(t.Plugin, &probe) {
			out = append(out, t)
		}
	}
	return out
}

// toolOpenToAll 判断插件是否完全没有访问限制（清单 + WebUI 覆盖都算）。

// toolOpenToAll 判断插件是否完全没有访问限制（清单 + WebUI 覆盖都算）。
func (m *Manager) toolOpenToAll(man *Manifest) bool {
	if man == nil {
		return false
	}
	users, groups, role := man.Access.AllowUsers, man.Access.AllowGroups, man.Access.RequireRole
	if m.Overrides != nil {
		if ov, ok := m.Overrides.Policy(man.ID); ok {
			users, groups, role = ov.AllowUsers, ov.AllowGroups, ov.RequireRole
		}
	}
	return len(users) == 0 && len(groups) == 0 && role == ""
}

// PluginHTTPResponse 是插件自定义页面接口的返回。

// CallTool 按名字找到提供者并执行。插件互调（host.call_capability）也走这里。
func (m *Manager) CallTool(ctx context.Context, name string, args map[string]any, s *session.Session) (json.RawMessage, error) {
	owner := m.toolOwner(name)
	if owner == nil {
		return nil, fmt.Errorf("工具 %s 不存在（没有插件声明它）", name)
	}
	// 带会话时按插件权限判定，防止 AI 调工具绕过「权限」页的配置。
	// 这里同样只查权限名单，不看触发门禁（命令要不要前缀跟能不能用工具无关）。
	if s != nil {
		probe := *s
		probe.HasPrefix = true
		probe.Mentioned = true
		if !m.allow(owner.Manifest, &probe) {
			return nil, fmt.Errorf("当前用户无权使用 %s 的工具", owner.Manifest.ID)
		}
	} else if !m.toolOpenToAll(owner.Manifest) {
		// 没传会话就无法校验用户权限：受限工具一律拒绝，
		// 否则插件可以省略 session 绕过「权限」页
		return nil, fmt.Errorf("调用 %s 的工具必须带会话（该插件配置了访问限制）", owner.Manifest.ID)
	}
	params := map[string]any{"name": name, "args": args}
	if s != nil {
		params["session"] = s
	}
	return m.acquireCall(ctx, owner.Manifest.ID, "plugin.call_tool", params, 8*time.Second)
}

func (m *Manager) toolOwner(name string) *Host {
	m.mu.RLock()
	ids := append([]string(nil), m.order...)
	hosts := make(map[string]*Host, len(m.hosts))
	for k, v := range m.hosts {
		hosts[k] = v
	}
	metas := make(map[string]*Manifest, len(m.metas))
	for k, v := range m.metas {
		metas[k] = v
	}
	m.mu.RUnlock()

	// 先找正在跑的
	for _, id := range ids {
		if h := hosts[id]; h != nil {
			for _, t := range h.Manifest.Tools {
				if t.Name == name {
					return h
				}
			}
		}
	}
	// 没在运行的：按清单找到它，调用时再拉起
	for _, id := range ids {
		if hosts[id] != nil {
			continue
		}
		man := metas[id]
		if man == nil {
			continue
		}
		owned := false
		for _, t := range man.Tools {
			if t.Name == name {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		h, err := m.ensureRunning(ctx, id)
		cancel()
		if err == nil && h != nil {
			return h
		}
	}
	return nil
}

// CheckAccess 按插件 id 判定某个会话能否使用它。
// 给插件用（host.check_permission），让 AI 在调工具前先问一句。

// CheckAccess 按插件 id 判定某个会话能否使用它。
// 给插件用（host.check_permission），让 AI 在调工具前先问一句。
func (m *Manager) CheckAccess(pluginID string, s *session.Session) bool {
	if s == nil {
		return false
	}
	m.mu.RLock()
	man := m.metas[pluginID]
	m.mu.RUnlock()
	if man == nil {
		return false
	}
	return m.allow(man, s)
}

// AllAccess 返回所有插件的生效配置，供插件读取（例如 AI 插件展示能力范围）。

// AllAccess 返回所有插件的生效配置，供插件读取（例如 AI 插件展示能力范围）。
func (m *Manager) AllAccess() map[string]map[string]any {
	out := make(map[string]map[string]any)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, man := range m.metas {
		users := man.Access.AllowUsers
		groups := man.Access.AllowGroups
		prefix := true
		if man.Access.RequirePrefix != nil {
			prefix = *man.Access.RequirePrefix
		}
		mention := false
		if man.Access.RequireMention != nil {
			mention = *man.Access.RequireMention
		}
		if m.Overrides != nil {
			if ov, ok := m.Overrides.Policy(id); ok {
				users, groups = ov.AllowUsers, ov.AllowGroups
				if ov.RequirePrefix != nil {
					prefix = *ov.RequirePrefix
				}
				if ov.RequireMention != nil {
					mention = *ov.RequireMention
				}
			}
		}
		out[id] = map[string]any{
			"allow_users":     users,
			"allow_groups":    groups,
			"require_prefix":  prefix,
			"require_mention": mention,
		}
	}
	return out
}

// ListCapabilities 返回能力名 → 提供者列表（已按优先级排序），供 WebUI 展示"谁接管了这个能力"。
