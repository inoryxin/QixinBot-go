package plugin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 共享宿主验收（用 sdk/node/multi-host.mjs + 演示插件，真实端到端）。

const cohostDemoA = `import { serve } from '../_sdk/qixin.mjs'
serve({
  async handle_message({ session }) {
    if (session.text === 'boom') {
      setTimeout(() => { throw new Error('demo-a 崩了') }, 20)
    }
    return { handled: true, from: 'a' }
  },
})
`

const cohostDemoB = `import { serve } from '../_sdk/qixin.mjs'
serve({
  async handle_message({ session }) {
    return { handled: true, from: 'b' }
  },
})
`

func setupCohost(t *testing.T) *SharedHost {
	t.Helper()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	sdkDir := filepath.Join(repo, "sdk", "node")
	multiHost := filepath.Join(repo, "sdk", "node", "multi-host.mjs")
	dir := t.TempDir()

	for _, m := range []struct{ id, code string }{
		{"demo-a", cohostDemoA},
		{"demo-b", cohostDemoB},
	} {
		pdir := filepath.Join(dir, "plugins", m.id)
		if err := os.MkdirAll(filepath.Join(pdir, "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		sdkDest := filepath.Join(pdir, "_sdk")
		if err := os.MkdirAll(sdkDest, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"qixin.mjs", "worker-entry.mjs"} {
			b, err := os.ReadFile(filepath.Join(sdkDir, f))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sdkDest, f), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(pdir, "app", "index.js"), []byte(m.code), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath, err := NewSharedHostConfig(dir, sdkDir, []CohostMember{
		{ID: "demo-a", Dir: filepath.Join(dir, "plugins", "demo-a")},
		{ID: "demo-b", Dir: filepath.Join(dir, "plugins", "demo-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sh, err := StartSharedHost(multiHost, cfgPath, SharedHostOptions{}, func(ctx context.Context, pluginID, method string, params json.RawMessage) (any, *RPCError) {
		return map[string]any{}, nil
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Stop)
	return sh
}

func TestSharedHostRouting(t *testing.T) {
	sh := setupCohost(t)
	ctx := context.Background()
	for _, id := range []string{"demo-a", "demo-b"} {
		if err := sh.WaitPluginReady(ctx, id); err != nil {
			t.Fatalf("%s 未就绪: %v", id, err)
		}
		if _, err := sh.Call(ctx, id, "plugin.initialize", map[string]any{"plugin_id": id}); err != nil {
			t.Fatalf("%s initialize: %v", id, err)
		}
	}
	// 路由正确性：from 字段证明帧送到了对的 worker
	ra, err := sh.Call(ctx, "demo-a", "plugin.handle_message", map[string]any{"session": map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	var outA struct {
		Handled bool   `json:"handled"`
		From    string `json:"from"`
	}
	_ = json.Unmarshal(ra, &outA)
	if outA.From != "a" {
		t.Fatalf("demo-a 的响应来自错误的 worker: %+v", outA)
	}
	rb, err := sh.Call(ctx, "demo-b", "plugin.handle_message", map[string]any{"session": map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	var outB struct {
		From string `json:"from"`
	}
	_ = json.Unmarshal(rb, &outB)
	if outB.From != "b" {
		t.Fatalf("demo-b 的响应来自错误的 worker: %+v", outB)
	}
}

func TestSharedHostCrashIsolation(t *testing.T) {
	sh := setupCohost(t)
	ctx := context.Background()
	for _, id := range []string{"demo-a", "demo-b"} {
		if err := sh.WaitPluginReady(ctx, id); err != nil {
			t.Fatalf("%s 未就绪: %v", id, err)
		}
		if _, err := sh.Call(ctx, id, "plugin.initialize", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	// B 先确认正常
	if _, err := sh.Call(ctx, "demo-b", "plugin.ping", nil); err != nil {
		t.Fatal(err)
	}
	// 打崩 A（handler 返回后定时器崩溃 → worker 退出 → multi-host 自动重启）
	_, _ = sh.Call(ctx, "demo-a", "plugin.handle_message", map[string]any{"session": map[string]any{"text": "boom"}})

	// 崩溃窗口内 B 不受影响
	for i := 0; i < 3; i++ {
		if _, err := sh.Call(ctx, "demo-b", "plugin.ping", nil); err != nil {
			t.Fatalf("崩溃窗口内 demo-b 第 %d 次 ping 失败: %v", i+1, err)
		}
	}

	// A 自动重启后重新就绪（WaitPluginReady 的 ping 循环兜住重启时序）
	readyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sh.WaitPluginReady(readyCtx, "demo-a"); err != nil {
		t.Fatalf("demo-a 重启后未就绪: %v", err)
	}
	if _, err := sh.Call(ctx, "demo-a", "plugin.initialize", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	ra, err := sh.Call(ctx, "demo-a", "plugin.handle_message", map[string]any{"session": map[string]any{"text": "back"}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		From string `json:"from"`
	}
	_ = json.Unmarshal(ra, &out)
	if out.From != "a" {
		t.Fatalf("重启后的 demo-a 路由错误: %+v", out)
	}
}

func TestSharedHostStopStart(t *testing.T) {
	sh := setupCohost(t)
	ctx := context.Background()
	for _, id := range []string{"demo-a", "demo-b"} {
		if err := sh.WaitPluginReady(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// 停 A：ping 应失败，B 正常
	if err := sh.StopPlugin("demo-a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := sh.PingPlugin(ctx, "demo-a"); err == nil {
		t.Fatal("stop 后 demo-a 的 ping 应该失败")
	}
	if err := sh.PingPlugin(ctx, "demo-b"); err != nil {
		t.Fatal("stop demo-a 不该影响 demo-b")
	}
	// 再拉起
	if err := sh.StartPlugin("demo-a"); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sh.WaitPluginReady(readyCtx, "demo-a"); err != nil {
		t.Fatalf("start 后 demo-a 未就绪: %v", err)
	}
}
