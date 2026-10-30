package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DepInfo 是依赖管理页展示的一行。
type DepInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

var (
	pyPkgRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`)
	npmPkgRe = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
	depVerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+!*-]{0,40}$`)
)

func validPluginID(id string) bool {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return false
	}
	return true
}

// depsTarget 返回 kind/scope 对应的安装根目录。
//   python shared: <data>/deps/python
//   node   shared: <data>/deps/node（npm --prefix，包在 node_modules 下）
//   plugin python: <runtime>/<id>/deps/python
//   plugin node:   <runtime>/<id>/deps（npm --prefix，包在 node_modules 下）
func (m *Manager) depsTarget(kind, scope, pluginID string) (string, error) {
	if kind != "python" && kind != "node" {
		return "", fmt.Errorf("未知依赖类型 %q（只支持 python / node）", kind)
	}
	switch scope {
	case "shared":
		if m.SharedDepsDir == "" {
			return "", fmt.Errorf("共享依赖目录未配置")
		}
		if kind == "python" {
			return filepath.Join(m.SharedDepsDir, "python"), nil
		}
		return filepath.Join(m.SharedDepsDir, "node"), nil
	case "plugin":
		if !validPluginID(pluginID) {
			return "", fmt.Errorf("插件 id 不合法")
		}
		dir := filepath.Join(m.Dir, pluginID)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("插件 %s 的运行目录不存在", pluginID)
		}
		if kind == "python" {
			return filepath.Join(dir, "deps", "python"), nil
		}
		return filepath.Join(dir, "deps"), nil
	default:
		return "", fmt.Errorf("未知安装范围 %q（shared / plugin）", scope)
	}
}

// InstallDependency 用框架配置的 pip / npm 安装依赖。
// 只允许安装到共享目录或指定插件目录，包名/版本严格校验，避免命令注入。
func (m *Manager) InstallDependency(ctx context.Context, kind, scope, pluginID, pkg, version string) (string, error) {
	pkg = strings.TrimSpace(pkg)
	version = strings.TrimSpace(version)
	if kind == "python" {
		if !pyPkgRe.MatchString(pkg) {
			return "", fmt.Errorf("python 包名不合法: %q", pkg)
		}
	} else if !npmPkgRe.MatchString(strings.ToLower(pkg)) {
		return "", fmt.Errorf("node 包名不合法: %q", pkg)
	}
	if version != "" && !depVerRe.MatchString(version) {
		return "", fmt.Errorf("版本号不合法: %q", version)
	}
	target, err := m.depsTarget(kind, scope, pluginID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if kind == "python" {
		py := RuntimeOverride("python")
		if py == "" {
			for _, name := range []string{"python", "python3", "python.exe"} {
				if p, err := exec.LookPath(name); err == nil {
					py = p
					break
				}
			}
		}
		if py == "" {
			return "", fmt.Errorf("找不到 Python，请先在「运行时环境」里配置")
		}
		arg := pkg
		if version != "" {
			arg += "==" + version
		}
		cmd = exec.CommandContext(runCtx, py, "-m", "pip", "install",
			"--target", target, "--upgrade", "--no-cache-dir", "--only-binary=:all:", arg)
	} else {
		npm := ""
		if node := RuntimeOverride("node"); node != "" {
			cand := filepath.Join(filepath.Dir(node), "npm.cmd")
			if _, err := os.Stat(cand); err == nil {
				npm = cand
			} else {
				cand = filepath.Join(filepath.Dir(node), "npm")
				if _, err := os.Stat(cand); err == nil {
					npm = cand
				}
			}
		}
		if npm == "" {
			for _, name := range []string{"npm.cmd", "npm"} {
				if p, err := exec.LookPath(name); err == nil {
					npm = p
					break
				}
			}
		}
		if npm == "" {
			return "", fmt.Errorf("找不到 npm，请先在「运行时环境」里配置 Node")
		}
		arg := pkg
		if version != "" {
			arg += "@" + version
		}
		cmd = exec.CommandContext(runCtx, npm, "install", "--prefix", target,
			"--no-audit", "--no-fund", "--ignore-scripts", arg)
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if runCtx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("安装超时（5 分钟）")
	}
	if err != nil {
		if len(text) > 1200 {
			text = text[len(text)-1200:]
		}
		return text, fmt.Errorf("安装失败: %w\n%s", err, text)
	}
	if len(text) > 1200 {
		text = text[len(text)-1200:]
	}
	return text, nil
}

// ListDependencies 列出已安装的依赖。
func (m *Manager) ListDependencies(kind, scope, pluginID string) ([]DepInfo, error) {
	target, err := m.depsTarget(kind, scope, pluginID)
	if err != nil {
		return nil, err
	}
	if kind == "python" {
		return listPythonDeps(target), nil
	}
	return listNodeDeps(target), nil
}

func listPythonDeps(target string) []DepInfo {
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil
	}
	out := make([]DepInfo, 0)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".dist-info") {
			continue
		}
		di := DepInfo{Name: strings.TrimSuffix(e.Name(), ".dist-info")}
		if i := strings.LastIndex(di.Name, "-"); i > 0 {
			di.Version = di.Name[i+1:]
			di.Name = di.Name[:i]
		}
		// 优先从 METADATA 里取准确名字/版本
		if f, err := os.Open(filepath.Join(target, e.Name(), "METADATA")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, "Name: ") {
					di.Name = strings.TrimSpace(strings.TrimPrefix(line, "Name: "))
				} else if strings.HasPrefix(line, "Version: ") {
					di.Version = strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
				}
			}
			f.Close()
		}
		out = append(out, di)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func listNodeDeps(target string) []DepInfo {
	root := filepath.Join(target, "node_modules")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]DepInfo, 0)
	add := func(dir, name string) {
		raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return
		}
		var pkg struct {
			Version string `json:"version"`
		}
		_ = json.Unmarshal(raw, &pkg)
		out = append(out, DepInfo{Name: name, Version: pkg.Version})
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "@") {
			subs, err := os.ReadDir(filepath.Join(root, e.Name()))
			if err != nil {
				continue
			}
			for _, sub := range subs {
				if sub.IsDir() {
					add(filepath.Join(root, e.Name(), sub.Name()), e.Name()+"/"+sub.Name())
				}
			}
			continue
		}
		if e.Name() == ".bin" || e.Name() == ".package-lock.json" {
			continue
		}
		add(filepath.Join(root, e.Name()), e.Name())
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}
