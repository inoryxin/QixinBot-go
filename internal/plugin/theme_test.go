package plugin

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archive/zip"
)

func themeTestInstaller(t *testing.T, allowUnsigned bool) *Installer {
	t.Helper()
	return NewInstaller(t.TempDir(), t.TempDir(), nil, allowUnsigned,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

const themeManifest = `id = "sakura"
name = "🌸 樱花庭园"
version = "1.0.0"
kind = "theme"
description = "测试"

[theme.light]
bg = "#fff0f5"
accent = "#e75480"

[theme.dark]
bg = "#221820"
`

func TestParseThemeManifest(t *testing.T) {
	man, err := ParseThemeManifest([]byte(themeManifest))
	if err != nil {
		t.Fatal(err)
	}
	if man.ID != "sakura" || man.Name != "🌸 樱花庭园" {
		t.Fatalf("基础字段解析错误: %+v", man)
	}
	if man.Light["bg"] != "#fff0f5" || man.Dark["bg"] != "#221820" {
		t.Fatalf("色板解析错误: %+v", man)
	}

	// 不是主题包
	if _, err := ParseThemeManifest([]byte("id = \"x\"\nkind = \"plugin\"\n")); err == nil {
		t.Fatal("kind=plugin 应该被拒")
	}
	// 不认识的色板键：宁可拒掉也不能悄悄带上
	bad := strings.Replace(themeManifest, "accent = \"#e75480\"", "url = \"https://evil\"", 1)
	if _, err := ParseThemeManifest([]byte(bad)); err == nil {
		t.Fatal("未知键应该被拒")
	}
	// 不是颜色的值：防止 CSS 注入
	bad = strings.Replace(themeManifest, "bg = \"#fff0f5\"", "bg = \"red; } body{background:url(https://evil)\"", 1)
	if _, err := ParseThemeManifest([]byte(bad)); err == nil {
		t.Fatal("非 hex 值应该被拒")
	}
}

func buildThemeZip(t *testing.T, manifest string, extra map[string]string) []byte {
	t.Helper()
	var buf strings.Builder
	zw := zip.NewWriter(&buf)
	if err := addZipFile(zw, "manifest.toml", manifest); err != nil {
		t.Fatal(err)
	}
	for name, content := range extra {
		if err := addZipFile(zw, name, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

func TestInstallThemeZip(t *testing.T) {
	themesDir := t.TempDir()

	// 未签名 + 生产模式：拒绝
	inst := themeTestInstaller(t, false)
	_, err := inst.InstallThemeZip(buildThemeZip(t, themeManifest, map[string]string{
		"theme.css": "body{}",
	}), themesDir)
	if err == nil || !strings.Contains(err.Error(), "没有签名") {
		t.Fatalf("未签名包应该被拒: %v", err)
	}

	// 开发模式：放行，落盘 + 白名单外的文件被拒
	inst = themeTestInstaller(t, true)
	_, err = inst.InstallThemeZip(buildThemeZip(t, themeManifest, map[string]string{
		"theme.css": "body{color:pink}",
		"assets/":   "",
		"evil.sh":   "#!/bin/sh",
	}), themesDir)
	if err == nil || !strings.Contains(err.Error(), "不允许的文件") {
		t.Fatalf("白名单外的文件应该被拒: %v", err)
	}

	id, err := inst.InstallThemeZip(buildThemeZip(t, themeManifest, map[string]string{
		"theme.css": "body{color:pink}",
	}), themesDir)
	if err != nil {
		t.Fatal(err)
	}
	if id != "sakura" {
		t.Fatalf("返回的 id 应该是 sakura，实际 %q", id)
	}
	if _, err := os.Stat(filepath.Join(themesDir, "sakura", "theme.css")); err != nil {
		t.Fatal("theme.css 应该被解压")
	}

	// 再次安装（升级路径）：原子替换
	if _, err := inst.InstallThemeZip(buildThemeZip(t, themeManifest, map[string]string{
		"theme.css": "body{color:deeppink}",
	}), themesDir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(themesDir, "sakura", "theme.css"))
	if !strings.Contains(string(data), "deeppink") {
		t.Fatal("重装后 theme.css 应该被替换")
	}

	// 白名单验证不受 "./" 前缀干扰
	if _, err := inst.InstallThemeZip(buildThemeZip(t, themeManifest, map[string]string{
		"./theme.css": "body{}",
	}), themesDir); err != nil {
		t.Fatalf("./ 前缀的文件名应该被接受: %v", err)
	}
}
