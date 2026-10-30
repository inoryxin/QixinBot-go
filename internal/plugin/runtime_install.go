package plugin

import (
	"archive/zip"
	"bytes"
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

var runtimeSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// 运行时包的解压限制。比插件包宽（一个 node 运行时就上百 MB），
// 但必须有界：256MB 的压缩包展开出几百 GB 就是解压炸弹。
const (
	maxRuntimeZipFiles = 20000
	maxRuntimeZipFile  = 1 << 30  // 单文件 1 GB
	maxRuntimeZipTotal = 4 << 30 // 总共 4 GB
)

// InstalledRuntime 是「依赖」页展示的已安装运行时。
type InstalledRuntime struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // node / python
	Version   string `json:"version"`
	Platform  string `json:"platform"` // windows-x64 / linux-x64
	Exec      string `json:"exec"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Source    string `json:"source"` // 框架自带 / 系统
	Current   bool   `json:"current"`
}

// CurrentPlatform 返回运行时常量使用的平台名。
func CurrentPlatform() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "x86"
	}
	return runtime.GOOS + "-" + arch
}

// ZipKind 读取 zip 里的 manifest kind，用来在插件上传入口识别运行时包。
func ZipKind(raw []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		if f.Name != "manifest.toml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		if err != nil {
			return "", err
		}
		var man Manifest
		if err := toml.Unmarshal(data, &man); err != nil {
			return "", fmt.Errorf("manifest.toml 解析失败: %w", err)
		}
		return man.Kind, nil
	}
	return "", fmt.Errorf("包里没有 manifest.toml")
}

// InstallRuntimeZip 安装 kind="runtime" 的包。
// 结构：runtimes/<kind>/<version>/<platform>/，manifest.toml 留在目录里做元数据。
func (m *Manager) InstallRuntimeZip(raw []byte) (*InstalledRuntime, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("不是合法的 zip: %w", err)
	}
	var man *Manifest
	for _, f := range zr.File {
		if f.Name != "manifest.toml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		var v Manifest
		if err := toml.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("manifest.toml 解析失败: %w", err)
		}
		man = &v
		break
	}
	if man == nil {
		return nil, fmt.Errorf("包里没有 manifest.toml")
	}
	if man.Kind != "runtime" {
		return nil, fmt.Errorf("不是运行时包（kind=%q）", man.Kind)
	}
	if man.Runtime == nil || (man.Runtime.Kind != "node" && man.Runtime.Kind != "python") {
		return nil, fmt.Errorf("运行时包缺少 [runtime] kind=node|python")
	}
	if man.Runtime.Version == "" || man.Runtime.Platform == "" {
		return nil, fmt.Errorf("运行时包缺少 [runtime] version / platform")
	}
	if len(man.Exec) == 0 {
		return nil, fmt.Errorf("运行时包缺少 exec")
	}
	if m.RuntimeRoot == "" {
		return nil, fmt.Errorf("运行时目录未配置")
	}

	kind := man.Runtime.Kind
	version := strings.TrimSpace(man.Runtime.Version)
	platform := strings.TrimSpace(man.Runtime.Platform)
	if !runtimeSegmentRe.MatchString(version) {
		return nil, fmt.Errorf("运行时 version 不合法: %q", version)
	}
	if !runtimeSegmentRe.MatchString(platform) {
		return nil, fmt.Errorf("运行时 platform 不合法: %q", platform)
	}
	if platform != CurrentPlatform() {
		return nil, fmt.Errorf("运行时平台 %s 与本机 %s 不匹配，已拒绝安装", platform, CurrentPlatform())
	}
	target := filepath.Join(m.RuntimeRoot, kind, version, platform)
	rootAbs, err := filepath.Abs(m.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(rootAbs, targetAbs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("运行时目标路径越界: %s", targetAbs)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}

	// 同一运行时目标串行安装；临时目录唯一，失败时保留旧版本。
	m.runtimeInstallMu.Lock()
	defer m.runtimeInstallMu.Unlock()

	tmp, err := os.MkdirTemp(filepath.Dir(target), ".install-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if len(zr.File) > maxRuntimeZipFiles {
		return nil, fmt.Errorf("压缩包文件数过多（%d > %d）", len(zr.File), maxRuntimeZipFiles)
	}
	var totalUnpacked int64
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if name == "META-INF" || strings.HasPrefix(name, "META-INF/") || strings.HasSuffix(name, "/") {
			continue
		}
		clean := filepath.Clean(strings.ReplaceAll(name, "/", string(os.PathSeparator)))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("包内有非法路径 %q", f.Name)
		}
		dest := filepath.Join(tmp, clean)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		w, err := os.Create(dest)
		if err != nil {
			rc.Close()
			return nil, err
		}
		// 上限多读 1 字节用来发现截断：LimitReader 会把超限悄悄截断，
		// 截断的解释器装上去就是"运行时损坏"
		n, err := io.Copy(w, io.LimitReader(rc, maxRuntimeZipFile+1))
		w.Close()
		rc.Close()
		if err != nil {
			return nil, err
		}
		if n > maxRuntimeZipFile {
			return nil, fmt.Errorf("包内文件超过 %d MB 上限: %s", maxRuntimeZipFile>>20, f.Name)
		}
		totalUnpacked += n
		if totalUnpacked > maxRuntimeZipTotal {
			return nil, fmt.Errorf("压缩包解压后超过 %d GB 上限", maxRuntimeZipTotal>>30)
		}
		// zip 不保留可执行位；Linux/macOS 的 bin/ 和 .so 要自己补
		if !strings.HasPrefix(platform, "windows") {
			rel := filepath.ToSlash(clean)
			if strings.HasPrefix(rel, "bin/") || strings.Contains(rel, "/bin/") || strings.HasSuffix(rel, ".so") || strings.Contains(rel, ".so.") {
				_ = os.Chmod(dest, 0o755)
			}
		}
	}
	tmpExec := filepath.Join(tmp, filepath.FromSlash(man.Exec[0]))
	if _, err := os.Stat(tmpExec); err != nil {
		return nil, fmt.Errorf("运行时包里找不到 exec: %s", man.Exec[0])
	}

	// 旧目录先挪走，失败可回滚；成功后删备份。
	backup := target + ".old"
	var hadOld bool
	if _, err := os.Stat(target); err == nil {
		_ = os.RemoveAll(backup)
		if err := os.Rename(target, backup); err != nil {
			return nil, fmt.Errorf("无法替换旧运行时（可能仍被占用）: %w", err)
		}
		hadOld = true
	}
	if err := os.Rename(tmp, target); err != nil {
		if hadOld {
			_ = os.Rename(backup, target)
		}
		return nil, fmt.Errorf("安装运行时失败（旧版本已保留）: %w", err)
	}
	if hadOld {
		_ = os.RemoveAll(backup)
	}

	info := &InstalledRuntime{
		ID:       kind + "-" + version + "-" + platform,
		Kind:     kind,
		Version:  version,
		Platform: platform,
		Exec:     filepath.Join(target, filepath.FromSlash(man.Exec[0])),
		Path:     target,
	}
	if _, err := os.Stat(info.Exec); err != nil {
		return info, fmt.Errorf("运行时已解压，但 exec 不存在：%s", info.Exec)
	}
	if err := m.SetRuntimePath(kind, info.Exec); err != nil {
		return info, err
	}
	info.Current = true
	info.SizeBytes = dirSize(target)
	writeRuntimeSize(target, info.SizeBytes)
	return info, nil
}

// ListRuntimes 扫描 runtimes/<kind>/<version>/<platform>。
func (m *Manager) ListRuntimes() []InstalledRuntime {
	out := make([]InstalledRuntime, 0)
	if m.RuntimeRoot == "" {
		return out
	}
	kinds, _ := os.ReadDir(m.RuntimeRoot)
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		vers, _ := os.ReadDir(filepath.Join(m.RuntimeRoot, k.Name()))
		for _, v := range vers {
			if !v.IsDir() {
				continue
			}
			plats, _ := os.ReadDir(filepath.Join(m.RuntimeRoot, k.Name(), v.Name()))
			for _, p := range plats {
				if !p.IsDir() || strings.HasPrefix(p.Name(), ".") ||
					strings.HasSuffix(p.Name(), ".tmp") || strings.HasSuffix(p.Name(), ".old") {
					continue
				}
				dir := filepath.Join(m.RuntimeRoot, k.Name(), v.Name(), p.Name())
				exec := ""
				id := k.Name() + "-" + v.Name() + "-" + p.Name()
				var man Manifest
				raw, err := os.ReadFile(filepath.Join(dir, "manifest.toml"))
				if err == nil && toml.Unmarshal(raw, &man) == nil && man.Runtime != nil {
					id = man.ID
					if len(man.Exec) > 0 {
						exec = filepath.Join(dir, filepath.FromSlash(man.Exec[0]))
					}
				}
				if exec == "" {
					exec = inferRuntimeExec(dir, k.Name())
				}
				if exec == "" {
					continue // 没有可执行文件，不是完整运行时
				}
				cur := RuntimeOverride(k.Name()) == exec
				out = append(out, InstalledRuntime{
					ID: id, Kind: k.Name(), Version: v.Name(), Platform: p.Name(),
					Exec: exec, Path: dir, SizeBytes: runtimeSize(dir), Source: "框架自带", Current: cur,
				})
			}
		}
	}
	return out
}

// SetRuntimePath 更新 data/runtime.json 并立即生效。
func (m *Manager) SetRuntimePath(kind, path string) error {
	stateDir := filepath.Dir(m.RuntimeRoot)
	file := filepath.Join(stateDir, "runtime.json")
	cur := map[string]string{}
	if raw, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(raw, &cur); err != nil {
			// 解析失败就整体重写；坏一半的文件之前会被静默忽略，
			// 管理员看不到为什么"装好的运行时重启后又回到系统解释器"
			m.Log.Warn("runtime.json 损坏，忽略旧内容重写", "err", err)
		}
	}
	cur[kind] = path
	raw, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	// tmp + fsync + rename：写一半断电后不至于把好文件截断成空壳
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, file); err != nil {
		return err
	}
	SetRuntimeOverrides(cur)
	if kind == "node" {
		m.SetEnv("QIXIN_NODE", path)
	} else {
		m.SetEnv("QIXIN_PYTHON", path)
	}
	return nil
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// inferRuntimeExec 在 manifest 丢了时按约定找解释器，避免运行时从列表消失。
func inferRuntimeExec(dir, kind string) string {
	candidates := []string{"node.exe", "bin/node", "python.exe", "bin/python3.12", "bin/python3", "bin/python"}
	for _, c := range candidates {
		p := filepath.Join(dir, filepath.FromSlash(c))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			if kind == "node" && (c == "node.exe" || c == "bin/node") {
				return p
			}
			if kind == "python" && c != "node.exe" && c != "bin/node" {
				return p
			}
		}
	}
	return ""
}

// runtimeSize 读取安装时缓存的大小；老目录第一次读时补算并落盘。
func runtimeSize(dir string) int64 {
	file := filepath.Join(dir, ".qixin-runtime.json")
	if raw, err := os.ReadFile(file); err == nil {
		var meta struct {
			SizeBytes int64 `json:"size_bytes"`
		}
		if json.Unmarshal(raw, &meta) == nil && meta.SizeBytes > 0 {
			return meta.SizeBytes
		}
	}
	n := dirSize(dir)
	writeRuntimeSize(dir, n)
	return n
}

func writeRuntimeSize(dir string, n int64) {
	raw, _ := json.Marshal(map[string]int64{"size_bytes": n})
	_ = os.WriteFile(filepath.Join(dir, ".qixin-runtime.json"), raw, 0o644)
}

// PruneIncompatibleRuntimes 删除非当前平台的运行时目录和残留 .tmp。
// 启动时调用：本机用不了的东西不留在列表里占位置。
func (m *Manager) PruneIncompatibleRuntimes() {
	if m.RuntimeRoot == "" {
		return
	}
	kinds, _ := os.ReadDir(m.RuntimeRoot)
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		vers, _ := os.ReadDir(filepath.Join(m.RuntimeRoot, k.Name()))
		for _, v := range vers {
			if !v.IsDir() {
				continue
			}
			plats, _ := os.ReadDir(filepath.Join(m.RuntimeRoot, k.Name(), v.Name()))
			for _, p := range plats {
				if !p.IsDir() {
					continue
				}
				if p.Name() == CurrentPlatform() && !strings.HasSuffix(p.Name(), ".tmp") {
					continue
				}
				dir := filepath.Join(m.RuntimeRoot, k.Name(), v.Name(), p.Name())
				if err := os.RemoveAll(dir); err == nil {
					m.Log.Info("清理不可用运行时", "path", dir, "platform", p.Name(), "current", CurrentPlatform())
				}
			}
			_ = os.Remove(filepath.Join(m.RuntimeRoot, k.Name(), v.Name()))
		}
	}
}
