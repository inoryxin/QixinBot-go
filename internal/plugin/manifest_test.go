package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// testManifest 造一份最小可用的清单。
func testManifest(dir string, withPlatform string) string {
	path := filepath.Join(dir, "manifest.toml")
	content := `id = "test-plugin"
name = "测试"
version = "1.0.0"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
description = "测试用"

[[commands]]
id = "t.hello"
usage = "/hello"
aliases = ["!hi"]

[runtime]
kind = "node"
`
	if withPlatform != "" {
		content += `platform = "` + withPlatform + `"
`
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
	return path
}

func TestLoadManifestStripsUsagePrefix(t *testing.T) {
	// 目录名必须和 manifest.id 一致，所以建一层子目录
	dir := filepath.Join(t.TempDir(), "test-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testManifest(dir, "")

	man, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	// usage 和 alias 里手写的 / 和 ! 都要被剥掉
	if man.Commands[0].Usage != "hello" {
		t.Fatalf("usage 应该被剥成 hello，实际 %q", man.Commands[0].Usage)
	}
	if man.Commands[0].Aliases[0] != "hi" {
		t.Fatalf("alias 应该被剥成 hi，实际 %q", man.Commands[0].Aliases[0])
	}
}

func TestPlatformOK(t *testing.T) {
	cases := []struct {
		declared string
		want     bool
	}{
		{"", true},                                     // 不声明 = 全平台
		{"windows|linux", true},                        // 当前系统在列表里
		{"nonesuch", false},                            // 不在列表
	}
	for _, c := range cases {
		if got := platformOK(c.declared); got != c.want {
			t.Errorf("platformOK(%q) = %v，期望 %v", c.declared, got, c.want)
		}
	}
}

func TestStripPrefixSymbol(t *testing.T) {
	cases := map[string]string{
		"/hello":    "hello",
		"!hello":    "hello",
		"#hello":    "hello",
		".hello":    "hello",
		"hello":     "hello",
		"a/b":       "a/b",   // 中间的斜杠不能动
		"":          "",
		"  /hello ": "hello", // 前后的空格
	}
	for in, want := range cases {
		if got := stripPrefixSymbol(in); got != want {
			t.Errorf("stripPrefixSymbol(%q) = %q，期望 %q", in, got, want)
		}
	}
}
