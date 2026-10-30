package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckRequires 检查插件声明的外部依赖。
// sharedDeps 是 data/deps；runtimeExec 是 node/python 的可执行文件（可空）。
func CheckRequires(man *Manifest, dir string, installed func(string) bool, sharedDeps string, runtimeExec map[string]string) error {
	for _, r := range man.Requires {
		value := r.Value
		if value == "" {
			value = r.Name
		}
		if value == "" {
			continue
		}

		ok := false
		switch r.Kind {
		case "plugin":
			if installed != nil {
				ok = installed(value)
			}
		case "env":
			ok = os.Getenv(value) != ""
		case "file":
			path := value
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			if _, err := os.Stat(path); err == nil {
				ok = true
			}
		case "python", "node":
			ok = hasRuntimeDep(r, dir, sharedDeps, runtimeExec[r.Kind])
		default: // binary
			if _, err := exec.LookPath(value); err == nil {
				ok = true
			}
		}

		if !ok {
			hint := r.Hint
			if hint == "" {
				switch r.Kind {
				case "env":
					hint = fmt.Sprintf("请设置环境变量 %s（可在 config.toml 的 [plugin.env] 里配置）", value)
				case "plugin":
					hint = fmt.Sprintf("需要先安装插件 %s（对应的 .anzu 包），装好后启用本插件即可", value)
				case "file":
					hint = fmt.Sprintf("找不到文件 %s", value)
				case "python":
					hint = fmt.Sprintf("运行时/共享/插件目录里没有 Python 包 %s；去控制台「依赖」页安装", value)
				case "node":
					hint = fmt.Sprintf("运行时/共享/插件目录里没有 Node 包 %s；去控制台「依赖」页安装", value)
				default:
					hint = fmt.Sprintf("请安装 %s，并确保它在 PATH 里", value)
				}
			}
			name := r.Name
			if name == "" {
				name = value
			}
			return fmt.Errorf("缺少依赖 %s：%s", name, hint)
		}
	}
	return nil
}

func hasRuntimeDep(r RequireSpec, pluginDir, sharedDeps, runtimeExec string) bool {
	name := strings.TrimSpace(r.Value)
	if name == "" {
		name = strings.TrimSpace(r.Name)
	}
	if name == "" {
		return true
	}
	var roots []string
	if r.Kind == "node" {
		roots = append(roots,
			filepath.Join(pluginDir, "deps", "node_modules"),
			filepath.Join(sharedDeps, "node", "node_modules"),
		)
		if runtimeExec != "" {
			roots = append(roots, filepath.Join(filepath.Dir(runtimeExec), "node_modules"))
		}
		for _, root := range roots {
			if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err == nil && fi.IsDir() {
				return true
			}
		}
		return false
	}

	roots = append(roots, filepath.Join(pluginDir, "deps", "python"))
	if sharedDeps != "" {
		roots = append(roots, filepath.Join(sharedDeps, "python"))
	}
	if runtimeExec != "" {
		rt := filepath.Dir(runtimeExec)
		roots = append(roots, filepath.Join(rt, "python"), filepath.Join(rt, "Lib", "site-packages"))
	}
	module := strings.TrimSpace(r.Module)
	if module == "" {
		module = name
	}
	modulePath := filepath.FromSlash(strings.ReplaceAll(module, ".", "/"))
	norm := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, modulePath)); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(root, modulePath+".py")); err == nil {
			return true
		}
		// dist-info 目录：Pillow-13.0.0.dist-info
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				low := strings.ToLower(e.Name())
				if strings.HasPrefix(low, norm+"-") && strings.HasSuffix(low, ".dist-info") {
					return true
				}
			}
		}
	}
	return false
}
