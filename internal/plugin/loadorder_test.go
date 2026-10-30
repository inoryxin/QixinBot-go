package plugin

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// 加载顺序：小的先、同 order 按 id、组内排序（service 那轮不影响普通插件那轮）。

// orderOf 模拟 Load 里的排序逻辑，方便单测（不依赖真实插件目录）。
func orderOf(loadOrder map[string]int, ids []string) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := loadOrder[out[i]], loadOrder[out[j]]
		if oi != oj {
			return oi < oj
		}
		return out[i] < out[j]
	})
	return out
}

func TestLoadOrderSmallerFirst(t *testing.T) {
	order := map[string]int{"canvas": 10, "llm": 20, "ai-chat": 30}
	got := orderOf(order, []string{"ai-chat", "llm", "canvas"})
	want := []string{"canvas", "llm", "ai-chat"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不对: 期望 %v 实际 %v", want, got)
		}
	}
}

func TestLoadOrderDefaultZero(t *testing.T) {
	// 都没设 = 全 0 → 按 id 字母序
	order := map[string]int{}
	got := orderOf(order, []string{"zeta", "alpha", "beta"})
	want := []string{"alpha", "beta", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不对: 期望 %v 实际 %v", want, got)
		}
	}
}

func TestLoadOrderMixed(t *testing.T) {
	// 设了的（可以是负数）排前面，没设的排后面
	order := map[string]int{"early": -10, "late": 100}
	got := orderOf(order, []string{"normal", "late", "early", "aaa"})
	want := []string{"early", "aaa", "normal", "late"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不对: 期望 %v 实际 %v", want, got)
		}
	}
}

func TestLoadOrderSameValueStableById(t *testing.T) {
	order := map[string]int{"b": 5, "a": 5, "c": 5}
	got := orderOf(order, []string{"c", "b", "a"})
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("同 order 应该按 id 排: 期望 %v 实际 %v", want, got)
		}
	}
}

func TestLoadOrderParsedFromManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `id = "svc"
name = "服务"
version = "1.0.0"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
description = "测试"
kind = "service"
provides = ["svc.thing@1"]
load_order = 7

[runtime]
kind = "node"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if man.LoadOrder != 7 {
		t.Fatalf("load_order 应该解析成 7，实际 %d", man.LoadOrder)
	}
}
