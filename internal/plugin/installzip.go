package plugin

// 上传安装：.anzu 压缩包校验与解压。

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// InstallZip 从 zip 安装插件，返回插件 id。已存在同名插件时拒绝，避免误覆盖。
func (m *Manager) InstallZip(r io.Reader, size int64) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("不是合法的 zip: %w", err)
	}
	id, err := peekZipID(zr)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(m.Dir, id)
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("插件 %s 已存在，请先删除再上传", id)
	}
	if m.Inst == nil {
		return "", fmt.Errorf("安装器未就绪，无法校验签名")
	}

	// **先写进投放区，再走统一的 Sync**。
	// 不能像以前那样直接解压到运行区：
	//   1. 那样会绕过签名校验（谁都能传个包进来）
	//   2. 包没落进投放区，重启后 Sync 会把运行目录当"孤儿"移走，插件就没了
	pkgPath := filepath.Join(m.PackagesDir, id+".anzu")
	if err := os.MkdirAll(m.PackagesDir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])

	// 写包 + 同步作为一个事务：期间监视器不能插进来
	m.Inst.LockSync()
	defer m.Inst.UnlockSync()

	// 拿锁前可能两个请求都看到"还没装"：锁内再确认一次，
	// 保证"同名已存在就拒绝覆盖"的约定
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("插件 %s 已存在，请先删除再上传", id)
	}

	tmpPath := pkgPath + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmpPath, raw, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, pkgPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	// 只删"内容还是本次上传"的包，避免误删另一条流程的新包
	removeOurs := func() {
		cur, err := os.ReadFile(pkgPath)
		if err != nil {
			return
		}
		got := sha256.Sum256(cur)
		if hex.EncodeToString(got[:]) == want {
			_ = os.Remove(pkgPath)
		}
	}

	if _, err := m.Inst.SyncWhileLocked(); err != nil {
		removeOurs()
		return "", err
	}
	if _, err := os.Stat(dest); err != nil {
		removeOurs()
		return "", fmt.Errorf("安装失败：运行目录没有生成（看日志里的安装错误）")
	}

	m.Log.Info("插件已安装", "plugin", id)
	return id, nil
}

// peekZipID 从 zip 里的清单读出插件 id，同时判断是否有多余顶层目录。

// peekZipID 从 zip 里的清单读出插件 id，同时判断是否有多余顶层目录。
// peekZipKind 读包内 manifest.toml 的 kind（runtime 包走更宽的解压上限）。
// 读不到 manifest 或没写 kind 时返回空（调用方按普通插件处理）。
func peekZipKind(zr *zip.Reader) string {
	for _, f := range zr.File {
		if f.Name != "manifest.toml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ""
		}
		raw, err := io.ReadAll(io.LimitReader(rc, 256<<10))
		_ = rc.Close()
		if err != nil {
			return ""
		}
		var man struct {
			Kind string `toml:"kind"`
		}
		if _, err := toml.Decode(string(raw), &man); err != nil {
			return ""
		}
		return man.Kind
	}
	return ""
}

func peekZipID(zr *zip.Reader) (string, error) {
	for _, f := range zr.File {
		base := strings.ToLower(filepath.Base(f.Name))
		if base != "manifest.toml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()

		var head struct {
			ID string `toml:"id"`
		}
		if _, err := toml.Decode(string(data), &head); err != nil {
			return "", fmt.Errorf("manifest.toml 解析失败: %w", err)
		}
		if !dirNameRe.MatchString(head.ID) {
			return "", fmt.Errorf("manifest.toml 里的 id %q 不合法", head.ID)
		}
		return head.ID, nil
	}
	return "", fmt.Errorf("压缩包里没有 manifest.toml")
}

// 安装包解压限制：防止异常压缩包打满磁盘。

// 安装包解压限制：防止异常压缩包打满磁盘。
const (
	maxZipFiles     = 20000
	maxZipFile      = 64 << 20  // 插件/主题单文件 64 MB
	maxZipTotal     = 512 << 20 // 插件/主题解压总量 512 MB
	runtimeZipFile  = 256 << 20 // runtime 包单文件上限（解释器二进制超 64MB，与 qixinpack 打包侧对齐）
	runtimeZipTotal = 1 << 30   // runtime 包解压总量 1 GB
)

// extractZip 解压到 target，跳过公共顶层目录，并拒绝路径穿越。

// extractZip 解压到 target，跳过公共顶层目录，并拒绝路径穿越。
func extractZip(zr *zip.Reader, target string, maxFile, maxTotal int64) error {
	prefix := commonTopDir(zr)
	cleanTarget := filepath.Clean(target)

	if len(zr.File) > maxZipFiles {
		return fmt.Errorf("压缩包文件数过多（%d > %d）", len(zr.File), maxZipFiles)
	}
	var total int64
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, prefix)
		name = strings.TrimPrefix(name, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if strings.HasPrefix(name, "META-INF/") {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("压缩包含非法路径: %s", f.Name)
		}
		dest := filepath.Join(cleanTarget, clean)
		if !strings.HasPrefix(dest, cleanTarget+string(os.PathSeparator)) {
			return fmt.Errorf("压缩包含非法路径: %s", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = rc.Close()
			return err
		}
		// 单文件超限要报错，不能静默截断后当成功
		n, copyErr := io.Copy(out, io.LimitReader(rc, maxFile+1))
		_ = out.Close()
		_ = rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if n > maxFile {
			return fmt.Errorf("压缩包内文件过大（> %d MB）: %s", maxFile>>20, f.Name)
		}
		total += n
		if total > maxTotal {
			return fmt.Errorf("压缩包解压总量过大（> %d MB）", maxTotal>>20)
		}
	}
	return nil
}

// commonTopDir 返回所有文件共同的顶层目录（带结尾斜杠）；没有则返回空。

// commonTopDir 返回所有文件共同的顶层目录（带结尾斜杠）；没有则返回空。
func commonTopDir(zr *zip.Reader) string {
	top := ""
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "/")
		if name == "" {
			continue
		}
		idx := strings.Index(name, "/")
		if idx < 0 {
			return ""
		}
		first := name[:idx+1]
		if top == "" {
			top = first
		} else if top != first {
			return ""
		}
	}
	return top
}

func removeString(list []string, target string) []string {
	out := list[:0]
	for _, v := range list {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

// ---- 状态持久化 ----
