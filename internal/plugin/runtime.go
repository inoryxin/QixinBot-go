package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RuntimeInfo 是插件运行环境的检测结果。
type RuntimeInfo struct {
	Kind    string `json:"kind"`    // node / python / binary
	Command string `json:"command"` // 实际用的可执行文件
	Version string `json:"version,omitempty"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// DetectRuntime 判断插件需要什么运行时，并检查是否可用。
// 优先用 manifest 的 [runtime] 声明；没声明就从 exec[0] 推断。
func DetectRuntime(man *Manifest, dir string) RuntimeInfo {
	kind := "binary"
	if man.Runtime != nil && man.Runtime.Kind != "" {
		kind = man.Runtime.Kind
	} else {
		kind = inferRuntimeKind(man.Exec)
	}

	if len(man.Exec) == 0 {
		return RuntimeInfo{Kind: kind, OK: false, Message: "manifest 里没有 exec"}
	}

	cmd := man.Exec[0]

	// 装好的运行时包 / 界面配的解释器优先于 PATH。
	// 之前这里直接 LookPath("node")：运行时装进 data/runtimes 并写进
	// runtime.json 后预检仍报"PATH 里找不到 node"，插件永远起不来——
	// 真正 fork 时用的 ResolveExecutable 会看 override，两边必须一致。
	if resolved := ResolveExecutable(cmd); resolved != cmd {
		if st, err := os.Stat(resolved); err == nil && !st.IsDir() {
			cmd = resolved
		}
		// override 失效（运行时被删）时继续按原命令走 PATH，别直接判死
	}

	// 带路径的（自带运行时）：只检查文件在不在
	if strings.ContainsAny(cmd, `/\`) {
		path := cmd
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			return RuntimeInfo{
				Kind: kind, Command: cmd, OK: false,
				Message: fmt.Sprintf("找不到 %s（检查插件包的 deps/ 和 exec 声明）", path),
			}
		}
		return RuntimeInfo{Kind: kind, Command: path, OK: true}
	}

	// 走 PATH
	path, err := exec.LookPath(cmd)
	if err != nil {
		return RuntimeInfo{Kind: kind, Command: cmd, OK: false, Message: missingMessage(kind, cmd)}
	}

	info := RuntimeInfo{Kind: kind, Command: path, OK: true}
	if v := probeVersion(path, kind); v != "" {
		info.Version = v
		if need := man.Runtime; need != nil && need.Version != "" {
			if !versionSatisfies(v, need.Version) {
				info.OK = false
				info.Message = fmt.Sprintf("检测到 %s %s，但插件要求 %s", kind, v, need.Version)
			}
		}
		return info
	}

	// 解释器跑不出 --version：多半是 Windows 应用商店那个占位符
	// （文件存在、LookPath 也成功，但执行返回 9009）
	if isInterpreter(kind) {
		info.OK = false
		info.Message = fmt.Sprintf(
			"找到了 %s，但执行失败——可能是 Windows 应用商店的占位符。"+
				"请装真正的 %s，或在 config.toml 的 [plugin.env] 里把 PATH 指到安装目录",
			path, kind)
	}
	return info
}

// isInterpreter 判断是不是解释器（这类必须有可执行的 --version）
func isInterpreter(kind string) bool {
	switch kind {
	case "node", "python", "deno", "bun":
		return true
	}
	return false
}

// inferRuntimeKind 从 exec[0] 猜语言。
func inferRuntimeKind(exec []string) string {
	if len(exec) == 0 {
		return "binary"
	}
	name := strings.ToLower(filepath.Base(exec[0]))
	switch {
	case strings.HasPrefix(name, "node"):
		return "node"
	case strings.HasPrefix(name, "python"):
		return "python"
	case name == "deno":
		return "deno"
	case name == "bun":
		return "bun"
	case strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".mjs"):
		return "node"
	case strings.HasSuffix(name, ".py"):
		return "python"
	}
	return "binary"
}

func missingMessage(kind, cmd string) string {
	switch kind {
	case "node":
		return "PATH 里找不到 node。请安装 Node.js（建议 18+），" +
			"或在 config.toml 的 [plugin.env] 里设置 PATH；" +
			"也可以把 node 放进插件包的 deps/ 并把 exec 改成 [\"./deps/node\", ...]"
	case "python":
		return "PATH 里找不到 python。请安装 Python 3，" +
			"或在 config.toml 的 [plugin.env] 里设置 PATH"
	case "deno":
		return "PATH 里找不到 deno，请安装 Deno"
	case "bun":
		return "PATH 里找不到 bun，请安装 Bun"
	default:
		return fmt.Sprintf("PATH 里找不到可执行文件 %q", cmd)
	}
}

// probeVersion 跑 --version 拿版本号，失败就返回空。
func probeVersion(path, kind string) string {
	arg := "--version"
	if kind == "node" {
		arg = "--version"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, arg).Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	// "v18.19.0" / "Python 3.12.1" / "node v20.1.0"
	line = strings.TrimPrefix(line, "v")
	line = strings.TrimPrefix(line, "Python ")
	if i := strings.IndexAny(line, " ("); i > 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// versionSatisfies 支持 ">=18" / ">=3.10" / "18"（前缀匹配）。
func versionSatisfies(have, need string) bool {
	need = strings.TrimSpace(need)
	op := ""
	for _, prefix := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(need, prefix) {
			op = prefix
			need = strings.TrimSpace(need[len(prefix):])
			break
		}
	}
	if need == "" {
		return true
	}

	h, n := parseVersion(have), parseVersion(need)
	cmp := compareVersion(h, n)
	switch op {
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	default:
		return cmp == 0 || strings.HasPrefix(have, need)
	}
}

func parseVersion(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, 0, 3)
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func compareVersion(a, b []int) int {
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// platformOK 判断插件的 platform 声明是否兼容当前系统。
// 空 = 全平台；"windows|linux" 这种写法也认。
func platformOK(declared string) bool {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return true
	}
	for _, part := range strings.Split(declared, "|") {
		if strings.TrimSpace(part) == runtime.GOOS {
			return true
		}
	}
	return false
}

// RuntimeStatus 是给概览页展示的运行时状态。
type RuntimeStatus struct {
	Name    string
	Kind    string // node / python，供 WebUI 指定路径用
	Path    string // 实际用的可执行文件
	Source  string // 框架自带 / 系统 PATH / 手动
	OK      bool
	Version string
	Message string
}

// runtimeOverrides 是管理员手动指定的解释器路径（data/runtime.json）。
var (
	runtimeMu        sync.RWMutex
	runtimeOverrides = map[string]string{}
)

// SetRuntimeOverrides 设置/更新解释器路径（kind → 可执行文件）。
func SetRuntimeOverrides(m map[string]string) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	runtimeOverrides = map[string]string{}
	for k, v := range m {
		if strings.TrimSpace(v) != "" {
			runtimeOverrides[k] = strings.TrimSpace(v)
		}
	}
}

// RuntimeOverride 返回某个解释器的手动路径。
func RuntimeOverride(kind string) string {
	runtimeMu.RLock()
	defer runtimeMu.RUnlock()
	return runtimeOverrides[kind]
}

// DetectAllRuntimes 检测框架支持的几种运行时，供 WebUI 展示。
func DetectAllRuntimes() []RuntimeStatus {
	specs := []struct {
		name    string
		cmds    []string
		install string
	}{
		{"Node.js", []string{"node", "node.exe"}, "https://nodejs.org/"},
		{"Python", []string{"python", "python3", "python.exe"}, "https://www.python.org/"},
	}

	out := make([]RuntimeStatus, 0, len(specs))
	for _, sp := range specs {
		kind := "node"
		if sp.name == "Python" {
			kind = "python"
		}

		// 手动指定的路径优先（用户可能装了多个环境，想用特定的那个）
		if custom := RuntimeOverride(kind); custom != "" {
			if v := probeVersion(custom, kind); v != "" {
				src := "手动"
				if strings.Contains(strings.ToLower(custom), "runtimes") {
					src = "框架自带"
				}
				out = append(out, RuntimeStatus{Name: sp.name, Kind: kind, Path: custom, Source: src, OK: true, Version: v, Message: custom})
			} else {
				out = append(out, RuntimeStatus{
					Name: sp.name, Kind: kind, Path: custom, Source: "手动", OK: false,
					Message: "指定的路径用不了：" + custom + "（检查文件是否存在、能否执行 --version）",
				})
			}
			continue
		}

		found := false
		for _, cmd := range sp.cmds {
			path, err := exec.LookPath(cmd)
			if err != nil {
				continue
			}
			if v := probeVersion(path, kind); v != "" {
				out = append(out, RuntimeStatus{Name: sp.name, Kind: kind, Path: path, Source: "系统 PATH", OK: true, Version: v, Message: path})
				found = true
				break
			}
		}
		if !found {
			out = append(out, RuntimeStatus{
				Name: sp.name, OK: false,
				Message: "没装，或装的是 Windows 应用商店的占位符。安装地址：" + sp.install,
			})
		}
	}
	return out
}

// ResolveExecutable 把 node / python 这类通用命令换成管理员在运行时环境里指定的路径。
// exec[0] 是绝对路径或其它命令时原样返回。
func ResolveExecutable(name string) string {
	base := strings.ToLower(filepath.Base(name))
	switch base {
	case "node", "node.exe":
		if p := RuntimeOverride("node"); p != "" {
			return p
		}
	case "python", "python3", "python.exe", "python3.exe":
		if p := RuntimeOverride("python"); p != "" {
			return p
		}
	}
	return name
}
