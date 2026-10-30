package plugin

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// 信任列表热更新的测试。守的场景：
// 第三方发来包 → 签名者不被信任 → 管理员贴公钥进控制台 → 立刻能装（不重启）。

func newTestStore(t *testing.T, cfgKeys []string) *TrustStore {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewTrustStore(filepath.Join(dir, "trusted-keys.json"), cfgKeys, log)
}

func mustKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestTrustStoreMergesConfigAndAdded(t *testing.T) {
	cfgKey := mustKey(t)
	newKey := mustKey(t)
	ts := newTestStore(t, []string{cfgKey})

	if err := ts.Add(newKey); err != nil {
		t.Fatalf("添加失败: %v", err)
	}

	all := ts.All()
	want := len(OfficialKeys()) + 2
	if len(all) != want {
		t.Fatalf("应该是官方 %d + config 1 + 热加 1 = %d，实际 %d",
			len(OfficialKeys()), want, len(all))
	}
	// 官方排最前，其次 config
	if all[0] != OfficialKeys()[0] {
		t.Fatal("官方公钥应该排最前")
	}
}

func TestTrustStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted-keys.json")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := mustKey(t)

	ts := NewTrustStore(path, nil, log)
	if err := ts.Add(key); err != nil {
		t.Fatalf("添加失败: %v", err)
	}

	// 模拟重启：重新打开
	ts2 := NewTrustStore(path, nil, log)
	if ts2.Source(key) != TrustAdded {
		t.Fatalf("重启后热加的应该还在: %#v", ts2.All())
	}
}

func TestTrustStoreDeduplicates(t *testing.T) {
	key := mustKey(t)
	ts := newTestStore(t, []string{key})

	if err := ts.Add(key); err != nil {
		t.Fatalf("重复添加不该报错: %v", err)
	}
	want := len(OfficialKeys()) + 1
	if len(ts.All()) != want {
		t.Fatalf("应该是官方 %d + config 1 = %d，实际 %d",
			len(OfficialKeys()), want, len(ts.All()))
	}
}

func TestTrustStoreRejectsBadKey(t *testing.T) {
	ts := newTestStore(t, nil)
	if err := ts.Add("不是公钥"); err == nil {
		t.Fatal("格式不对应该报错")
	}
	if err := ts.Add(""); err == nil {
		t.Fatal("空串应该报错")
	}
}

func TestTrustStoreCannotRemoveConfigKey(t *testing.T) {
	cfgKey := mustKey(t)
	ts := newTestStore(t, []string{cfgKey})

	err := ts.Remove(cfgKey)
	if err == nil {
		t.Fatal("config 里的公钥不该能从控制台删掉")
	}
	if !strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("错误信息应该指向 config.toml: %v", err)
	}
}

func TestTrustStoreRemoveAdded(t *testing.T) {
	key := mustKey(t)
	ts := newTestStore(t, nil)
	_ = ts.Add(key)

	if err := ts.Remove(key); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	// 删完之后只剩官方内置的
	if len(ts.All()) != len(OfficialKeys()) {
		t.Fatalf("应该只剩官方 %d 个，实际 %d", len(OfficialKeys()), len(ts.All()))
	}
	if ts.Source(key) != "" {
		t.Fatalf("删掉的应该查不到来源，实际 %q", ts.Source(key))
	}
}

func TestInstallerHotUpdatesKeys(t *testing.T) {
	// 核心：SetTrustedKeys 之后立刻生效，不用重建 Installer
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	k1 := base64.StdEncoding.EncodeToString(pub1)
	k2 := base64.StdEncoding.EncodeToString(pub2)

	_, pkgDir, runtimeDir := setupInstaller(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inst := NewInstaller(pkgDir, runtimeDir, []string{k1}, false, log)

	if got := len(inst.trustedKeys()); got != 1 {
		t.Fatalf("初始应该有 1 个公钥，实际 %d", got)
	}

	if err := inst.SetTrustedKeys([]string{k1, k2}); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	if got := len(inst.trustedKeys()); got != 2 {
		t.Fatalf("热更新后应该有 2 个，实际 %d", got)
	}
	if got := len(inst.TrustedKeys()); got != 2 {
		t.Fatalf("对外列表也该是 2，实际 %d", got)
	}
}

func TestSignerInfoReadsFingerprint(t *testing.T) {
	// 包里带 signer.pub 时，框架能报出"谁签的"
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pkg := anzuWithSigner(t, "demo", priv)

	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatalf("读包失败: %v", err)
	}
	fp, key := signerInfo(zr)
	if fp == "" || key == "" {
		t.Fatal("应该能读出签名者")
	}
	if fp != KeyFingerprint(pub) {
		t.Fatalf("指纹对不上: %s vs %s", fp, KeyFingerprint(pub))
	}
}

func TestSignerInfoMissingReturnsEmpty(t *testing.T) {
	// 老包没有 signer.pub：返回空，不 panic
	pkg := anzuWithSigner(t, "demo", nil)
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatalf("读包失败: %v", err)
	}
	if fp, key := signerInfo(zr); fp != "" || key != "" {
		t.Fatalf("没有签名者时应该返回空: %s %s", fp, key)
	}
}

// anzuWithSigner 造一个包；priv 为 nil 时不写签名相关文件。
func anzuWithSigner(t *testing.T, id string, priv ed25519.PrivateKey) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifest := `id = "` + id + `"
name = "测试"
version = "1.0.0"
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

	if priv != nil {
		list := []byte("清单内容")
		sig := ed25519.Sign(priv, list)
		pub := priv.Public().(ed25519.PublicKey)
		_ = addZipFile(zw, "META-INF/manifest.sha256", string(list))
		_ = addZipFile(zw, "META-INF/signature.ed25519", string(sig))
		_ = addZipFile(zw, "META-INF/signer.pub",
			base64.StdEncoding.EncodeToString(pub)+"\n")
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestOfficialKeysAlwaysTrusted(t *testing.T) {
	// 空配置下官方公钥也必须生效——装官方插件不该要求用户改 config
	if len(OfficialKeys()) == 0 {
		t.Fatal("官方公钥不能为空")
	}
	ts := newTestStore(t, nil)
	if got := ts.All(); len(got) != len(OfficialKeys()) {
		t.Fatalf("空配置也应该有官方公钥，实际 %d 个", len(got))
	}
	for _, k := range OfficialKeys() {
		if ts.Source(k) != TrustOfficial {
			t.Fatalf("%s 应该是 official 来源", k)
		}
	}
}

func TestCannotRemoveOfficialKey(t *testing.T) {
	ts := newTestStore(t, nil)
	key := OfficialKeys()[0]
	err := ts.Remove(key)
	if err == nil {
		t.Fatal("官方公钥不该能从控制台删掉")
	}
	if !strings.Contains(err.Error(), "官方") {
		t.Fatalf("错误信息应该说明是官方公钥: %v", err)
	}
}

func TestOfficialKeysSurviveConfigAndHot(t *testing.T) {
	// 三层叠加：官方 + config + 热加，顺序和来源都要对
	cfgKey := mustKey(t)
	hotKey := mustKey(t)
	ts := newTestStore(t, []string{cfgKey})
	if err := ts.Add(hotKey); err != nil {
		t.Fatalf("添加失败: %v", err)
	}

	all := ts.All()
	if len(all) != len(OfficialKeys())+2 {
		t.Fatalf("层数不对: %d", len(all))
	}
	if ts.Source(hotKey) != TrustAdded {
		t.Fatal("热加的应该是 added")
	}
	if ts.Source(cfgKey) != TrustConfig {
		t.Fatal("config 的应该是 config")
	}
}
