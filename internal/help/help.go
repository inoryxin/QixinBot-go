package help

import (
	"fmt"
	"strings"

	"github.com/xin/onebot/internal/override"
	"github.com/xin/onebot/internal/perm"
	"github.com/xin/onebot/internal/registry"
	"github.com/xin/onebot/internal/session"
)

// FormatUsage 把清单里的用法加上命令前缀。插件声明时不带前缀，
// 由框架统一加，保证所有插件的显示一致。
func FormatUsage(usage, fallback string, prefixes []string) string {
	if usage == "" {
		usage = fallback
	}
	if len(prefixes) == 0 || prefixes[0] == "" {
		return usage
	}
	p := prefixes[0]
	if strings.HasPrefix(usage, p) {
		return usage
	}
	return p + usage
}

// visible 判断某条命令在当前会话下是否要出现在 /help。
// 三个条件：插件级开关、命令级 hidden、调用者权限。
func visible(f *registry.Feature, checker *perm.Checker, s *session.Session, ov *override.Store) bool {
	if ov != nil {
		if !ov.PluginVisible(f.Plugin) {
			return false
		}
		if h := ov.FeatureHidden(f.ID); h != nil {
			if *h {
				return false
			}
		} else if f.Hidden {
			return false
		}
	} else if f.Hidden {
		return false
	}
	return checker.CanSee(f.Policy, s)
}

// Render 生成一级菜单：只列插件名和编号，不展开命令。
// 想看细节就 /help <编号> 或 /help <插件名>。
func Render(reg *registry.Registry, checker *perm.Checker, s *session.Session, ov *override.Store, prefixes []string, idle []string) string {
	order, groups := collect(reg, checker, s, ov)
	if len(order) == 0 {
		return "当前没有可用的指令。"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "可用插件（%d 个）\n", len(order))
	for i, plugin := range order {
		list := groups[plugin]
		label := list[0].Label
		if label == "" {
			label = plugin
		}
		fmt.Fprintf(&b, "%d. %s — %d 条\n", i+1, label, len(list))
	}
	b.WriteString("\n输入 /help <编号> 或 /help <插件中文名> 看详细用法")
	if len(idle) > 0 {
		fmt.Fprintf(&b, "\n\n另有 %d 个插件没有指令：%s", len(idle), strings.Join(idle, "、"))
	}
	return b.String()
}

// collect 按插件分组并保持稳定顺序。
func collect(reg *registry.Registry, checker *perm.Checker, s *session.Session, ov *override.Store) ([]string, map[string][]*registry.Feature) {
	groups := make(map[string][]*registry.Feature)
	var order []string

	for _, f := range reg.All() {
		if f.Kind != registry.KindCommand || !visible(f, checker, s, ov) {
			continue
		}
		if _, ok := groups[f.Plugin]; !ok {
			order = append(order, f.Plugin)
		}
		groups[f.Plugin] = append(groups[f.Plugin], f)
	}
	return order, groups
}

// PluginAt 返回一级菜单里第 idx 个（从 1 开始）插件的 id。
func PluginAt(reg *registry.Registry, checker *perm.Checker, s *session.Session, ov *override.Store, idx int) (string, bool) {
	order, _ := collect(reg, checker, s, ov)
	if idx < 1 || idx > len(order) {
		return "", false
	}
	return order[idx-1], true
}

// findPlugin 判断某个 id 是否存在（哪怕它没有命令）。
func findPlugin(reg *registry.Registry, id string) (string, bool) {
	for _, f := range reg.All() {
		if f.Plugin == id {
			return id, true
		}
	}
	return "", false
}

// resolveByName 用中文名（label）反查插件 id，支持前缀匹配。
func resolveByName(reg *registry.Registry, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	// 先精确匹配 label
	for _, f := range reg.All() {
		if f.Label != "" && f.Label == name {
			return f.Plugin, true
		}
	}
	// 再前缀匹配
	for _, f := range reg.All() {
		if f.Label != "" && strings.HasPrefix(f.Label, name) {
			return f.Plugin, true
		}
	}
	return "", false
}

// RenderPlugin 只显示某个插件的指令。
func RenderPlugin(reg *registry.Registry, checker *perm.Checker, s *session.Session, plugin string, ov *override.Store, prefixes []string) (string, bool) {
	// 支持中文名：先按 id 找，找不到就拿 name 反查 id
	if _, ok := findPlugin(reg, plugin); !ok {
		if id, ok := resolveByName(reg, plugin); ok {
			plugin = id
		}
	}
	if ov != nil && !ov.PluginVisible(plugin) {
		return "", false
	}
	var b strings.Builder
	found := false
	for _, f := range reg.All() {
		if f.Plugin != plugin || f.Kind != registry.KindCommand {
			continue
		}
		if !visible(f, checker, s, ov) {
			continue
		}
		if !found {
			label := f.Label
			if label == "" {
				label = plugin
			}
			fmt.Fprintf(&b, "【%s】\n", label)
			found = true
		}
		usage := FormatUsage(f.Usage, f.ID, prefixes)
		if f.Description != "" {
			fmt.Fprintf(&b, "  %s — %s\n", usage, f.Description)
		} else {
			fmt.Fprintf(&b, "  %s\n", usage)
		}
	}
	if !found {
		return "", false
	}
	return strings.TrimRight(b.String(), "\n"), true
}
