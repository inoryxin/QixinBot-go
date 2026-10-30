package plugin

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 这些测试覆盖插件作者最依赖的流程：安装、升级（数据不丢）、卸载。
// 用真实的 zip 包，走真实的解压逻辑，不打桩。

// makeAnzu 造一个最小可用的 .anzu 包（zip）。
func makeAnzu(t *testing.T, id, version string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	manifest := `id = "` + id + `"
name = "测试插件"
version = "` + version + `"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
description = "集成测试用"

[[commands]]
id = "` + id + `.hello"
usage = "hello"

[runtime]
kind = "node"
`
	if err := addZipFile(zw, "manifest.toml", manifest); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := addZipFile(zw, name, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func addZipFile(zw *zip.Writer, name, content string) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(content))
	return err
}

// setupInstaller 准备一个临时环境：packages 目录 + runtime 目录。
func setupInstaller(t *testing.T) (*Installer, string, string) {
	t.Helper()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "plugins")
	runtimeDir := filepath.Join(root, "runtime")
	_ = os.MkdirAll(pkgDir, 0o755)
	_ = os.MkdirAll(runtimeDir, 0o755)
	inst := NewInstaller(pkgDir, runtimeDir, nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return inst, pkgDir, runtimeDir
}

func TestInstallWritesRuntimeDir(t *testing.T) {
	inst, pkgDir, runtimeDir := setupInstaller(t)

	pkg := makeAnzu(t, "demo", "1.0.0", map[string]string{
		"app/index.js": "console.log('hi')",
	})
	if err := os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), pkg, 0o644); err != nil {
		t.Fatal(err)
	}

	fresh, err := inst.Sync()
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if len(fresh) != 1 || fresh[0] != "demo" {
		t.Fatalf("应该装 1 个新插件，实际 %v", fresh)
	}

	// 运行区目录结构
	for _, p := range []string{"app/index.js", "manifest.toml", "install.json"} {
		if _, err := os.Stat(filepath.Join(runtimeDir, "demo", p)); err != nil {
			t.Fatalf("缺 %s: %v", p, err)
		}
	}
}

func TestUpgradeKeepsPluginData(t *testing.T) {
	// 这是最关键的回归：升级时插件写的数据不能被覆盖掉
	inst, pkgDir, runtimeDir := setupInstaller(t)

	v1 := makeAnzu(t, "demo", "1.0.0", map[string]string{"app/index.js": "v1"})
	_ = os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), v1, 0o644)
	if _, err := inst.Sync(); err != nil {
		t.Fatal(err)
	}

	// 假装插件写了数据
	dataFile := filepath.Join(runtimeDir, "demo", "data", "state.json")
	_ = os.MkdirAll(filepath.Dir(dataFile), 0o755)
	if err := os.WriteFile(dataFile, []byte(`{"counter":42}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 升级到 2.0.0
	v2 := makeAnzu(t, "demo", "2.0.0", map[string]string{"app/index.js": "v2"})
	_ = os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), v2, 0o644)
	if _, err := inst.Sync(); err != nil {
		t.Fatal(err)
	}

	// 代码换了
	got, err := os.ReadFile(filepath.Join(runtimeDir, "demo", "app", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Fatalf("代码应该更新成 v2，实际 %q", got)
	}

	// 数据还在
	got, err = os.ReadFile(dataFile)
	if err != nil {
		t.Fatalf("升级后数据文件没了: %v", err)
	}
	if string(got) != `{"counter":42}` {
		t.Fatalf("数据被覆盖了: %s", got)
	}
}

func TestUninstallMovesRuntimeDir(t *testing.T) {
	inst, pkgDir, runtimeDir := setupInstaller(t)

	pkg := makeAnzu(t, "demo", "1.0.0", map[string]string{"app/index.js": "x"})
	_ = os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), pkg, 0o644)
	if _, err := inst.Sync(); err != nil {
		t.Fatal(err)
	}

	// 删包 = 卸载
	if err := os.Remove(filepath.Join(pkgDir, "demo.anzu")); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Sync(); err != nil {
		t.Fatal(err)
	}

	// 运行区应该被改名成 .removed-*（数据保留，方便手动恢复）
	entries, _ := os.ReadDir(runtimeDir)
	found := false
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) > 9 && e.Name()[:9] == ".removed-" {
			found = true
		}
	}
	if !found {
		t.Fatalf("卸载后应该留下 .removed-* 目录，实际: %v", entries)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	inst, pkgDir, _ := setupInstaller(t)

	pkg := makeAnzu(t, "demo", "1.0.0", map[string]string{"app/index.js": "x"})
	_ = os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), pkg, 0o644)

	if _, err := inst.Sync(); err != nil {
		t.Fatal(err)
	}
	// 第二次装同一个包：不该报错，也不该再算"新装"
	fresh, err := inst.Sync()
	if err != nil {
		t.Fatalf("重复安装失败: %v", err)
	}
	if len(fresh) != 0 {
		t.Fatalf("第二次不该有新插件，实际 %v", fresh)
	}
}

// 签名策略的三种组合。这决定了"配了 trusted_keys 之后还能不能装自己的插件"。

func TestUnsignedRejectedWhenStrict(t *testing.T) {
	// 配了公钥 + 包没签名 + 不允许未签名 → 拒绝
	_, pkgDir, runtimeDir := setupInstaller(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 用真实的 ed25519 公钥（全零的会被 parseKeys 拒掉）
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	fakeKey := base64.StdEncoding.EncodeToString(pub)
	inst := NewInstaller(pkgDir, runtimeDir, []string{fakeKey}, false, log)
	t.Logf("公钥长度 %d，inst.keys 长度 %d", len(fakeKey), len(inst.keys))

	pkg := makeAnzu(t, "demo", "1.0.0", map[string]string{"app/index.js": "x"})
	_ = os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), pkg, 0o644)

	if _, err := inst.Sync(); err == nil {
		t.Fatal("严格模式不该接受未签名的包")
	}
}

func TestUnsignedAllowedInDevMode(t *testing.T) {
	// 配了公钥 + 包没签名 + 允许未签名 → 放行
	_, pkgDir, runtimeDir := setupInstaller(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	fakeKey := base64.StdEncoding.EncodeToString(pub)
	inst := NewInstaller(pkgDir, runtimeDir, []string{fakeKey}, true, log)

	pkg := makeAnzu(t, "demo", "1.0.0", map[string]string{"app/index.js": "x"})
	_ = os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), pkg, 0o644)

	if _, err := inst.Sync(); err != nil {
		t.Fatalf("开发模式应该放行未签名包: %v", err)
	}
}

func TestBadSignatureAlwaysRejected(t *testing.T) {
	// **签错了始终拒绝**——allow_unsigned 只放行"没签"，不放行"签错"
	_, pkgDir, runtimeDir := setupInstaller(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	fakeKey := base64.StdEncoding.EncodeToString(pub)
	inst := NewInstaller(pkgDir, runtimeDir, []string{fakeKey}, true, log) // 就算开了开发模式

	// 造一个"带签名但是假的"包
	pkg := makeAnzuSigned(t, "demo", "1.0.0")
	_ = os.WriteFile(filepath.Join(pkgDir, "demo.anzu"), pkg, 0o644)

	if _, err := inst.Sync(); err == nil {
		t.Fatal("签名无效的包必须拒绝，即使开了 allow_unsigned")
	}
}

// makeAnzuSigned 造一个带假签名的包。
func makeAnzuSigned(t *testing.T, id, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifest := `id = "` + id + `"
name = "测试"
version = "` + version + `"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
description = "x"

[[commands]]
id = "demo.hello"
usage = "hello"

[runtime]
kind = "node"
`
	_ = addZipFile(zw, "manifest.toml", manifest)
	_ = addZipFile(zw, "app/index.js", "x")
	_ = addZipFile(zw, "META-INF/manifest.sha256", "deadbeef")
	_ = addZipFile(zw, "META-INF/signature.ed25519", base64.StdEncoding.EncodeToString(make([]byte, 64)))
	_ = zw.Close()
	return buf.Bytes()
}

func TestDirSignatureIgnoresData(t *testing.T) {
	// 插件写 data/ 不该影响签名——否则定时任务每次存状态都会触发重载。
	//
	// 注意：dirSignature 用 mtime 参与哈希，Windows 上杀毒软件之类的
	// 后台扫描偶尔会更新 mtime。所以这里**显式把 mtime 钉死**，
	// 否则测试会随机失败（和代码正确性无关）。
	dir := filepath.Join(t.TempDir(), "p")
	_ = os.MkdirAll(filepath.Join(dir, "app"), 0o755)
	manifest := filepath.Join(dir, "manifest.toml")
	code := filepath.Join(dir, "app/index.js")
	_ = os.WriteFile(manifest, []byte(`id = "p"`), 0o644)
	_ = os.WriteFile(code, []byte("x"), 0o644)

	fixed := time.Unix(1700000000, 0)
	_ = os.Chtimes(manifest, fixed, fixed)
	_ = os.Chtimes(code, fixed, fixed)

	before := dirSignature(dir)

	// 模拟插件写状态（data/ 在签名里被跳过）
	_ = os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "data/state.json"), []byte(`{"n":1}`), 0o644)

	if after := dirSignature(dir); after != before {
		t.Fatalf("写 data/ 不该改变签名（before=%s after=%s）", before, after)
	}

	// 对照：改代码（mtime 也改）应该让签名变
	later := time.Unix(1700000100, 0)
	_ = os.WriteFile(code, []byte("y"), 0o644)
	_ = os.Chtimes(code, later, later)
	if dirSignature(dir) == before {
		t.Fatal("改代码应该改变签名")
	}
}
