package plugin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var exactVerRe = regexp.MustCompile(`^\d+(\.\d+){0,3}$`)

// ensureDownloadedDeps 处理 manifest 里 download=true 的 Python/Node 依赖：
// 缺了就装到插件自己的 deps/，不碰共享目录。失败直接返回，让插件进 failed。
func (m *Manager) ensureDownloadedDeps(ctx context.Context, man *Manifest, dir string) error {
	runtimes := map[string]string{"node": RuntimeOverride("node"), "python": RuntimeOverride("python")}
	for _, r := range man.Requires {
		if (r.Kind != "python" && r.Kind != "node") || !r.Download {
			continue
		}
		name := strings.TrimSpace(r.Value)
		if name == "" {
			name = strings.TrimSpace(r.Name)
		}
		if name == "" || hasRuntimeDep(r, dir, m.SharedDepsDir, runtimes[r.Kind]) {
			continue
		}
		version := strings.TrimSpace(r.Version)
		if !exactVerRe.MatchString(version) {
			version = "" // 范围版本交给 pip/npm 解析，不冒然拼 ==
		}
		m.Log.Info("自动安装插件依赖", "plugin", man.ID, "kind", r.Kind, "package", name, "version", version)
		if _, err := m.InstallDependency(ctx, r.Kind, "plugin", man.ID, name, version); err != nil {
			return fmt.Errorf("自动安装依赖 %s 失败：%w", name, err)
		}
	}
	return nil
}
