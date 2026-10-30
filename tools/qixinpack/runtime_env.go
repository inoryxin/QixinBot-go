package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

type runtimePackManifest struct {
	ID      string   `toml:"id"`
	Exec    []string `toml:"exec"`
	Runtime struct {
		Kind     string `toml:"kind"`
		Version  string `toml:"version"`
		Platform string `toml:"platform"`
	} `toml:"runtime"`
}

// runtimeSegRe 校验 version/platform 这类要拼进路径的段：
// 包是外部来的，manifest 里写个 "..\\..\\Windows\\启动项" 不能放行
var runtimeSegRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

func currentPlatform() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return runtime.GOOS + "-" + arch
}

// installRuntimePackages 把 -runtime 指定的运行时包解到开发工作区，
// 并写 data/runtime.json，让 dev 环境和生产用同一套 Node/Python + 依赖。
func installRuntimePackages(work string, packages []string) error {
	if len(packages) == 0 {
		return nil
	}
	runtimeJSON := filepath.Join(work, "data", "runtime.json")
	cur := map[string]string{}
	if raw, err := os.ReadFile(runtimeJSON); err == nil {
		_ = json.Unmarshal(raw, &cur)
	}
	for _, pkg := range packages {
		kind, exec, err := extractRuntimePack(work, pkg)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(pkg), err)
		}
		cur[kind] = exec
		fmt.Printf("==> 运行时 %s: %s\n", kind, exec)
	}
	raw, _ := json.MarshalIndent(cur, "", "  ")
	return os.WriteFile(runtimeJSON, raw, 0o644)
}

func extractRuntimePack(work, pkg string) (string, string, error) {
	zr, err := zip.OpenReader(pkg)
	if err != nil {
		return "", "", fmt.Errorf("不是合法的 .anzu: %w", err)
	}
	defer zr.Close()

	var man runtimePackManifest
	found := false
	for _, f := range zr.File {
		if f.Name != "manifest.toml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", "", err
		}
		raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return "", "", err
		}
		if err := toml.Unmarshal(raw, &man); err != nil {
			return "", "", fmt.Errorf("manifest.toml 解析失败: %w", err)
		}
		found = true
		break
	}
	if !found {
		return "", "", fmt.Errorf("包里没有 manifest.toml")
	}
	if man.Runtime.Kind != "node" && man.Runtime.Kind != "python" {
		return "", "", fmt.Errorf("不是 Node/Python 运行时包（kind=%q）", man.Runtime.Kind)
	}
	if man.Runtime.Platform != currentPlatform() {
		return "", "", fmt.Errorf("平台 %s 与本机 %s 不匹配", man.Runtime.Platform, currentPlatform())
	}
	if len(man.Exec) == 0 {
		return "", "", fmt.Errorf("缺少 exec")
	}
	target := filepath.Join(runtimeCacheDir(), man.Runtime.Kind, man.Runtime.Version, man.Runtime.Platform)
	execPath := filepath.Join(target, filepath.FromSlash(man.Exec[0]))
	if fi, err := os.Stat(execPath); err == nil && !fi.IsDir() {
		return man.Runtime.Kind, execPath, nil // 已解压过，直接复用
	}
	// 段校验 + 包含检查：解压根目录必须真的在缓存目录里
	if !runtimeSegRe.MatchString(man.Runtime.Version) {
		return "", "", fmt.Errorf("runtime version 不合法: %q", man.Runtime.Version)
	}
	if !runtimeSegRe.MatchString(man.Runtime.Platform) {
		return "", "", fmt.Errorf("runtime platform 不合法: %q", man.Runtime.Platform)
	}
	rootAbs, err := filepath.Abs(runtimeCacheDir())
	if err != nil {
		return "", "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	if rel, err := filepath.Rel(rootAbs, targetAbs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("运行时目标路径越界: %s", targetAbs)
	}
	if err := os.RemoveAll(target); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", "", err
	}
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if name == "META-INF" || strings.HasPrefix(name, "META-INF/") || strings.HasSuffix(name, "/") {
			continue
		}
		clean := filepath.Clean(strings.ReplaceAll(name, "/", string(os.PathSeparator)))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return "", "", fmt.Errorf("包内非法路径 %q", f.Name)
		}
		dest := filepath.Join(target, clean)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", "", err
		}
		w, err := os.Create(dest)
		if err != nil {
			rc.Close()
			return "", "", err
		}
		// 上限多读 1 字节发现截断，别把截断的解释器静默装上
		n, err := io.Copy(w, io.LimitReader(rc, 1<<30+1))
		w.Close()
		rc.Close()
		if err != nil {
			return "", "", err
		}
		if n > 1<<30 {
			return "", "", fmt.Errorf("包内文件超过 1GB 上限: %s", f.Name)
		}
		if !strings.HasPrefix(man.Runtime.Platform, "windows") {
			rel := filepath.ToSlash(clean)
			if strings.HasPrefix(rel, "bin/") || strings.HasSuffix(rel, ".so") || strings.Contains(rel, ".so.") {
				_ = os.Chmod(dest, 0o755)
			}
		}
	}
	if fi, err := os.Stat(execPath); err != nil || fi.IsDir() {
		return "", "", fmt.Errorf("解压后找不到 exec: %s", execPath)
	}
	return man.Runtime.Kind, execPath, nil
}

func runtimeCacheDir() string {
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "qixinpack", "runtimes")
	}
	return ".qixinpack-runtimes"
}
