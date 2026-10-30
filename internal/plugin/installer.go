package plugin

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	sigDir      = "META-INF/"
	sigList     = "META-INF/manifest.sha256"
	sigFile     = "META-INF/signature.ed25519"
	installMeta = "install.json"
)

// InstallInfo 记录一个插件的安装来源，写在运行区根目录。
type InstallInfo struct {
	ID            string `json:"id"`
	Version       string `json:"version"`
	Package       string `json:"package"`
	PackageSHA256 string `json:"package_sha256"`
	Signed        bool   `json:"signed"`
	InstalledAt   int64  `json:"installed_at"`
}

// Installer 把 plugins/*.anzu 解压到运行区。
type Installer struct {
	PackagesDir string // plugins/
	RuntimeDir  string // data/plugins/
	Keys        []string
	Log         *slog.Logger

	keys []ed25519.PublicKey

	// keyMu 保护 keys：管理员可以在控制台**热加**可信公钥，
	// 不用改 config 重启。
	keyMu sync.RWMutex

	// syncMu 串行化 Sync：网页上传和热插拔监视可能同时触发，
	// 两轮同时解压/替换同一个包会互相踩。
	syncMu sync.Mutex

	// allowUnsigned 来自 [plugin] allow_unsigned：
	// 配了 trusted_keys 后，是否放行**没签名**的包（开发用）。
	allowUnsigned bool

	// keyErr 记录可信公钥解析失败：只要配置了错误的公钥，
	// 就拒绝安装任何包，绝不静默进入"不验签"状态。
	keyErr error

	// BeforeUpgrade 在"要覆盖已存在的运行目录"之前调用。
	//
	// Windows 上插件进程占着目录，直接 rename 会失败
	// （The process cannot access the file）。所以升级前必须先停进程，
	// 解压完再由调用方重新加载。删除包（MissingRuntimeIDs）走的是另一条路。
	BeforeUpgrade func(id string)
	// AfterUpgrade 在升级流程结束后调用（**成功或失败都会调**），
	// 用来清掉"升级中"标记，避免失败时后续拉起被卡住。
	AfterUpgrade func(id string, err error)
}

// NewInstaller 创建安装器。
func NewInstaller(packagesDir, runtimeDir string, keys []string, allowUnsigned bool, log *slog.Logger) *Installer {
	inst := &Installer{
		PackagesDir:   packagesDir,
		RuntimeDir:    runtimeDir,
		Keys:          keys,
		allowUnsigned: allowUnsigned,
		Log:           log.With("component", "installer"),
	}
	if len(keys) > 0 {
		if err := inst.SetTrustedKeys(keys); err != nil {
			inst.keyErr = err
			inst.Log.Error("公钥配置错误，安装将被拒绝（不会跳过签名校验）", "err", err)
		}
	}
	return inst
}

// EffectiveKeys 是"实际参与校验的公钥" = 官方内置 + 传入的额外信任。
//
// 调用方（main）应该用 TrustStore.All() 得到合并列表，
// 它已经包含官方那部分；这里再兜一层，避免直接构造 Installer 的调用方漏掉官方信任。
func EffectiveKeys(extra []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(officialKeys)+len(extra))
	for _, list := range [][]string{officialKeys, extra} {
		for _, k := range list {
			k = trimKey(k)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// SetTrustedKeys 替换可信公钥列表（运行时生效，不用重启）。
// 传空列表表示"不校验签名"（开发模式）。
func (i *Installer) SetTrustedKeys(list []string) error {
	parsed, err := parseKeys(list)
	if err != nil {
		return err
	}
	i.keyMu.Lock()
	i.Keys = append([]string(nil), list...)
	i.keys = parsed
	i.keyMu.Unlock()
	return nil
}

// TrustedKeys 返回当前可信公钥（base64）。
func (i *Installer) TrustedKeys() []string {
	i.keyMu.RLock()
	defer i.keyMu.RUnlock()
	return append([]string(nil), i.Keys...)
}

// trustedKeys 是内部用的已解析公钥快照。
func (i *Installer) trustedKeys() []ed25519.PublicKey {
	i.keyMu.RLock()
	defer i.keyMu.RUnlock()
	return append([]ed25519.PublicKey(nil), i.keys...)
}

// Sync 扫描投放区，安装新增或变更的包，并把没有包的运行目录移走。
// 返回本次新安装（需要标记为未启用）的插件 id。
func (i *Installer) Sync() ([]string, error) {
	i.syncMu.Lock()
	defer i.syncMu.Unlock()
	if i.keyErr != nil {
		return nil, fmt.Errorf("可信公钥配置错误，拒绝安装: %w", i.keyErr)
	}
	return i.syncOnce()
}

// LockSync/UnlockSync 让上传流程把"写包 + 同步"合成一个事务，
// 避免监视器在中间插进来把新插件登记成禁用。
func (i *Installer) LockSync()   { i.syncMu.Lock() }
func (i *Installer) UnlockSync() { i.syncMu.Unlock() }

// SyncWhileLocked 在已经持有 syncMu 时执行同步。
// 和 Sync 一样先过 keyErr：上传接口必须遵守和投放区相同的红线，
// 否则"配错公钥 + allow_unsigned"会从网页上绕过验签。
func (i *Installer) SyncWhileLocked() ([]string, error) {
	if i.keyErr != nil {
		return nil, fmt.Errorf("可信公钥配置错误，拒绝安装: %w", i.keyErr)
	}
	return i.syncOnce()
}

func (i *Installer) syncOnce() ([]string, error) {
	if i.keyErr != nil {
		return nil, fmt.Errorf("可信公钥配置错误，拒绝安装: %w", i.keyErr)
	}

	// 包目录是不是本来就存在：MkdirAll 会把"消失了一瞬"的目录重建出来
	// （移动盘/网络盘抖动），空目录会让下面的孤儿清理把所有插件全移走
	pkgDirExisted := dirExists(i.PackagesDir)
	if err := os.MkdirAll(i.PackagesDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(i.RuntimeDir, 0o755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(i.PackagesDir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var fresh []string
	var firstErr error
	peekFailed := false

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".anzu") {
			continue
		}
		path := filepath.Join(i.PackagesDir, e.Name())
		fallbackID := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		id, changed, err := i.installOne(path)
		if err != nil {
			i.Log.Error("安装失败", "package", e.Name(), "err", err)
			seenID := id
			if seenID == "" {
				// 包根本读不开，不知道里面是什么 id：文件名只能当最坏情况的占位
				seenID = fallbackID
				peekFailed = true
			}
			// 包坏了不等于插件被删了：把 id 记入 seen，
			// 否则下面的清理会把已安装插件的运行目录改名移走
			seen[seenID] = true
			if i.AfterUpgrade != nil {
				i.AfterUpgrade(seenID, err)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", e.Name(), err)
			}
			continue
		}
		seen[id] = true
		if i.AfterUpgrade != nil {
			i.AfterUpgrade(id, nil)
		}
		if changed {
			fresh = append(fresh, id)
		}
	}

	// 运行区里没有对应包的目录：改名到 .removed-*，保留数据但不加载。
	// 有包读不开 / 包目录刚被重建时跳过这轮清理：
	// 这时"谁没有包"根本对不上账，宁可多留一个孤儿也不能误删健康插件。
	if !peekFailed && pkgDirExisted {
		rts, err := os.ReadDir(i.RuntimeDir)
		i.Log.Debug("Sync 扫描运行区", "packages", len(seen), "runtime", len(rts), "err", err, "seen", seen)
		if err == nil {
			for _, e := range rts {
				name := e.Name()
				if !e.IsDir() || strings.HasPrefix(name, ".") {
					continue
				}
				if seen[name] {
					continue
				}
				target := filepath.Join(i.RuntimeDir, ".removed-"+name+"-"+strconv.FormatInt(time.Now().UnixMilli(), 10))
				if err := os.Rename(filepath.Join(i.RuntimeDir, name), target); err != nil {
					i.Log.Warn("移除运行目录失败", "plugin", name, "err", err)
					continue
				}
				i.Log.Info("包已删除，运行目录已移走（数据保留）", "plugin", name, "to", filepath.Base(target))
			}
		}
	} else if peekFailed {
		i.Log.Warn("有插件包无法读取，本轮跳过孤儿目录清理（宁可错留不可误删）")
	} else {
		i.Log.Warn("包目录刚才不存在（可能被重建），本轮跳过孤儿目录清理")
	}

	if len(fresh) > 0 {
		i.Log.Info("安装完成", "plugins", fresh)
	}
	return fresh, firstErr
}

// readFileRetry 读插件包；Windows 上部署脚本可能正在覆盖它，
// 等待重试几次比直接失败好得多（之前会刷一堆"文件被占用"）。
func readFileRetry(path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		time.Sleep(time.Duration(120*(attempt+1)) * time.Millisecond)
	}
	return nil, lastErr
}

// renameRetry 重命名目录；Windows 上插件进程刚停时目录可能还被占用。
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 12; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(150*(i+1)) * time.Millisecond)
	}
	return err
}

// removeAllRetry 删除目录；旧运行区可能被句柄短暂占着。
func removeAllRetry(path string) {
	for i := 0; i < 8; i++ {
		if err := os.RemoveAll(path); err == nil {
			return
		}
		time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
	}
}

func (i *Installer) installOne(path string) (string, bool, error) {
	raw, err := readFileRetry(path)
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(raw)
	pkgHash := hex.EncodeToString(sum[:])

	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", false, fmt.Errorf("不是合法的 zip: %w", err)
	}

	// 从这里往后 id 已知：出错也要把 id 带回去，调用方的
	// 升级闸门 / 清理对账都按 id 走，按文件名猜会张冠李戴
	id, err := peekZipID(zr)
	if err != nil {
		return "", false, err
	}
	fail := func(err error) (string, bool, error) { return id, false, err }

	// 已安装且包没变 → 只跳过解压，不跳过信任判断。
	// install.json 在插件自己可写的运行目录里，不能当信任依据：
	// 之前在这里直接 return，改一下 install.json 里的哈希就能把
	// 未签名的新包塞进生产配置。这里只豁免"没签名"的准入检查
	// （保持老行为：开发模式装过的插件，收紧配置后还能继续跑），
	// 包只要带了签名就一定验签。
	skipExtract := false
	if old, err := readInstallInfo(i.RuntimeDir, id); err == nil && old.PackageSHA256 == pkgHash {
		skipExtract = true
	}

	trusted := i.trustedKeys()
	signed := false
	if len(trusted) > 0 {
		if !hasSignature(zr) {
			// 没签名：开发模式下放行，生产模式下拒绝
			// （已经装过的没签名包豁免，避免收紧配置后启动报错刷屏）
			if !i.allowUnsigned && !skipExtract {
				return fail(fmt.Errorf(
					"插件 %s 没有签名（开发时可设 [plugin] allow_unsigned = true 放行）", id))
			}
			if !skipExtract {
				i.Log.Warn("安装未签名插件（开发模式）", "plugin", id)
			}
		} else {
			if err := verifyPackage(zr, trusted); err != nil {
				// **签错了始终拒绝**——这个开关只放行"没签"，不放行"签错"
				//
				// 告诉管理员"是谁签的"：否则他拿着包也没法判断该不该信任。
				if fp, key := signerInfo(zr); fp != "" {
					return fail(fmt.Errorf(
						"签名校验失败: %w（签发者指纹 %s，公钥 %s；信任它就把这串加进 trusted_keys）",
						err, fp, key))
				}
				return fail(fmt.Errorf("签名校验失败: %w", err))
			}
			signed = true
		}
	} else {
		// 没有任何可信公钥：带签名的无法验证、未签名的也无从判断来源，
		// 生产模式下必须拒绝，不能"跳过校验直接安装"。
		if !i.allowUnsigned {
			return fail(fmt.Errorf(
				"没有可信公钥，拒绝安装 %s（请配置 [plugin] trusted_keys，或开发时显式 allow_unsigned = true）",
				filepath.Base(path)))
		}
		if hasSignature(zr) {
			i.Log.Warn("包带签名但没有可信公钥，跳过校验（开发模式）", "package", filepath.Base(path))
		} else {
			i.Log.Warn("安装未签名插件（开发模式，无可信公钥）", "plugin", id)
		}
	}

	if skipExtract {
		return id, false, nil
	}

	target := filepath.Join(i.RuntimeDir, id)
	isNew := !dirExists(target)

	// 升级：先把正在跑的进程停掉，否则 Windows 上没法换目录
	if !isNew && i.BeforeUpgrade != nil {
		i.BeforeUpgrade(id)
	}
	staging := filepath.Join(i.RuntimeDir, ".staging-"+id+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))

	// runtime 包的解释器二进制 >64MB：按 manifest kind 放宽单文件/总量上限（与打包侧对齐）
	limits := struct{ file, total int64 }{maxZipFile, maxZipTotal}
	if peekZipKind(zr) == "runtime" {
		limits.file, limits.total = runtimeZipFile, runtimeZipTotal
	}
	if err := extractZip(zr, staging, limits.file, limits.total); err != nil {
		_ = os.RemoveAll(staging)
		return fail(err)
	}

	// 校验结构：manifest 和 app/ 必须存在
	if _, err := os.Stat(filepath.Join(staging, "manifest.toml")); err != nil {
		_ = os.RemoveAll(staging)
		return fail(fmt.Errorf("包内缺少 manifest.toml"))
	}
	if _, err := os.Stat(filepath.Join(staging, "app")); err != nil {
		_ = os.RemoveAll(staging)
		return fail(fmt.Errorf("包内缺少 app/ 目录"))
	}

	old := ""
	if dirExists(target) {
		// 旧目录用 "." 前缀：管理器扫描时会跳过点开头的目录，
		// 否则 "<id>.old-xxx" 会被当成一个插件去加载（报 manifest 不存在）。
		old = filepath.Join(i.RuntimeDir, ".old-"+id+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		if err := renameRetry(target, old); err != nil {
			_ = os.RemoveAll(staging)
			return fail(err)
		}
	}
	// 重装前把旧运行区的 data/ 搬进新包，否则用户配置、自定义数据全丢。
	// 复制失败必须中止升级并把旧目录挪回去：旧目录马上会被删掉，
	// 它是用户数据的唯一副本——之前"只 Warn 然后照删"，
	// Windows 上杀软/备份占用一下文件，升级"成功"了数据没了。
	if old != "" {
		oldData := filepath.Join(old, "data")
		if dirExists(oldData) {
			if err := copyTree(oldData, filepath.Join(staging, "data")); err != nil {
				_ = os.RemoveAll(staging)
				_ = renameRetry(old, target)
				return fail(fmt.Errorf("保留旧数据失败，已取消升级: %w", err))
			}
		}
	}

	if err := renameRetry(staging, target); err != nil {
		_ = os.RemoveAll(staging)
		if old != "" {
			_ = renameRetry(old, target)
		}
		return fail(err)
	}
	if old != "" {
		removeAllRetry(old)
	}

	var man struct {
		Version string `toml:"version"`
	}
	if data, err := os.ReadFile(filepath.Join(target, "manifest.toml")); err == nil {
		_, _ = toml.Decode(string(data), &man)
	}

	info := InstallInfo{
		ID:            id,
		Version:       man.Version,
		Package:       filepath.Base(path),
		PackageSHA256: pkgHash,
		Signed:        signed,
		InstalledAt:   time.Now().Unix(),
	}
	if raw, err := json.MarshalIndent(info, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(target, installMeta), raw, 0o644)
	}

	i.Log.Info("插件已解压", "plugin", id, "version", man.Version, "signed", signed, "new", isNew)
	return id, isNew, nil
}

// verifyPackage 校验签名与所有文件哈希。
func verifyPackage(zr *zip.Reader, keys []ed25519.PublicKey) error {
	var list, sig []byte
	for _, f := range zr.File {
		switch f.Name {
		case sigList:
			list = readZipFile(f, 4<<20)
		case sigFile:
			sig = readZipFile(f, 4<<10)
		}
	}
	if len(list) == 0 {
		return fmt.Errorf("缺少 %s", sigList)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("签名长度不对")
	}

	verified := false
	for _, k := range keys {
		if ed25519.Verify(k, list, sig) {
			verified = true
			break
		}
	}
	if !verified {
		return fmt.Errorf("签名不匹配任何可信公钥")
	}

	// 逐个文件比对
	expected := map[string]string{}
	for _, line := range strings.Split(string(list), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			return fmt.Errorf("清单格式错误: %s", line)
		}
		expected[strings.TrimSpace(parts[1])] = strings.ToLower(parts[0])
	}

	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, sigDir) || strings.HasSuffix(f.Name, "/") {
			continue
		}
		want, ok := expected[f.Name]
		if !ok {
			return fmt.Errorf("清单里没有 %s", f.Name)
		}
		data := readZipFile(f, 256<<20)
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("文件被篡改: %s", f.Name)
		}
		delete(expected, f.Name)
	}
	if len(expected) > 0 {
		for name := range expected {
			return fmt.Errorf("清单里的 %s 不存在", name)
		}
	}
	return nil
}

func hasSignature(zr *zip.Reader) bool {
	for _, f := range zr.File {
		if f.Name == sigFile {
			return true
		}
	}
	return false
}

func readZipFile(f *zip.File, limit int64) []byte {
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, _ := io.ReadAll(io.LimitReader(rc, limit))
	return data
}

func parseKeys(list []string) ([]ed25519.PublicKey, error) {
	out := make([]ed25519.PublicKey, 0, len(list))
	for _, s := range list {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("公钥长度应为 %d，实际 %d", ed25519.PublicKeySize, len(raw))
		}
		out = append(out, ed25519.PublicKey(raw))
	}
	return out, nil
}

func readInstallInfo(runtimeDir, id string) (InstallInfo, error) {
	var info InstallInfo
	raw, err := os.ReadFile(filepath.Join(runtimeDir, id, installMeta))
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(raw, &info)
	return info, err
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		// 任何一步失败都要往上传：调用方（升级保留数据）必须知道
		// 复制没完成，否则会拿着不完整的新目录把旧目录删掉
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// BuildSignatureList 供打包工具使用：生成 manifest.sha256 的内容。
func BuildSignatureList(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(b.String())
}

// copyTree 递归复制目录内容（dst 已存在时合并）。
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

// signerInfo 从包里取出签名者公钥，返回（指纹，base64 公钥）。
// 老包没有 signer.pub，返回空字符串。
func signerInfo(zr *zip.Reader) (string, string) {
	for _, f := range zr.File {
		if f.Name != "META-INF/signer.pub" {
			continue
		}
		raw := strings.TrimSpace(string(readZipFile(f, 4<<10)))
		pub, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return "", ""
		}
		sum := sha256.Sum256(pub)
		return hex.EncodeToString(sum[:8]), raw
	}
	return "", ""
}

// VerifyPackage 只做签名/哈希校验，不解压；运行时包上传共用同一套信任链。
func (i *Installer) VerifyPackage(raw []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("不是合法的 zip: %w", err)
	}
	trusted := i.trustedKeys()
	if len(trusted) == 0 {
		return nil
	}
	if !hasSignature(zr) {
		if !i.allowUnsigned {
			return fmt.Errorf("包没有签名（开发时可设 [plugin] allow_unsigned = true 放行）")
		}
		return nil
	}
	return verifyPackage(zr, trusted)
}
