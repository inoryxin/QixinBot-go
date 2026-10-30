package plugin

import (
	"os"
	"testing"
)

// 回归：装了运行时包（override 指向绝对路径）时，
// DetectRuntime 必须认这个解释器，而不是报"PATH 里找不到 node"。
func TestDetectRuntimeHonorsOverride(t *testing.T) {
	dir := t.TempDir()
	fake := dir + "/node"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	SetRuntimeOverrides(map[string]string{"node": fake})
	t.Cleanup(func() { SetRuntimeOverrides(nil) })

	man := &Manifest{ID: "x", Exec: []string{"node", "app/index.js"}}
	rt := DetectRuntime(man, dir)
	if !rt.OK {
		t.Fatalf("装了运行时仍判缺：%s", rt.Message)
	}
	if rt.Command != fake {
		t.Fatalf("应使用 override 的路径，实际 %q", rt.Command)
	}
}
