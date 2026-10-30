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

	"github.com/xin/onebot/internal/perm"
	"github.com/xin/onebot/internal/registry"
)

// Manager 集成：cohost 插件经 SharedHost 启动与调用；独占插件不受影响。

func writeCohostDemoPlugin(t *testing.T, root, id, code string) {
	t.Helper()
	pdir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(pdir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pdir, "_sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `id = "` + id + `"
name = "` + id + `"
version = "1.0.0"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
kind = "plugin"
cohost = true

[[commands]]
id = "` + id + `.hi"
usage = "hi"
`
	if err := os.WriteFile(filepath.Join(pdir, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "app", "index.js"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, _ := filepath.Abs(filepath.Join("..", ".."))
	sdkDir := filepath.Join(repo, "sdk", "node")
	for _, f := range []string{"qixin.mjs", "worker-entry.mjs"} {
		b, err := os.ReadFile(filepath.Join(sdkDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pdir, "_sdk", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManagerCohostIntegration(t *testing.T) {
	repo, _ := filepath.Abs(filepath.Join("..", ".."))
	sdkDir := filepath.Join(repo, "sdk", "node")
	dir := t.TempDir()

	writeCohostDemoPlugin(t, dir, "co-a", `import { serve } from '../_sdk/qixin.mjs'
serve({
  async handle_message({ session }) {
    if (session.text === 'boom') {
      setTimeout(() => { throw new Error('co-a 崩了') }, 20)
    }
    return { handled: true, from: 'a' }
  },
})
`)
	writeCohostDemoPlugin(t, dir, "co-b", `import { serve } from '../_sdk/qixin.mjs'
serve({
  async handle_message({ session }) {
    return { handled: true, from: 'b' }
  },
})
`)
	// 独占插件（manifest 不声明 cohost）
	writeCohostDemoPlugin(t, dir, "exclusive", `import { serve } from '../_sdk/qixin.mjs'
serve({
  async handle_message({ session }) {
    return { handled: true, from: 'exclusive' }
  },
})`)
	if err := os.WriteFile(filepath.Join(dir, "exclusive", "manifest.toml"), []byte(`id = "exclusive"
name = "exclusive"
version = "1.0.0"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
kind = "plugin"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	mgr := NewManager(dir, filepath.Join(dir, "data"), filepath.Join(dir, "state.json"), reg,
		perm.New("allow_all", nil), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	mgr.SDKDir = sdkDir
	mgr.OnHostCall = func(ctx context.Context, man *Manifest, method string, params json.RawMessage) (any, *RPCError) {
		return map[string]any{}, nil
	}
	mgr.SetCohostEnabled(true)
	ctx := context.Background()
	t.Cleanup(func() { mgr.StopAll(context.Background()) })
	pool, err := mgr.Pools.Create("test", 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Pools.Assign("co-a", pool.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Pools.Assign("co-b", pool.ID); err != nil {
		t.Fatal(err)
	}

	if err := mgr.startPlugin(ctx, "co-a"); err != nil {
		t.Fatalf("co-a 启动: %v", err)
	}
	if err := mgr.startPlugin(ctx, "co-b"); err != nil {
		t.Fatalf("co-b 启动: %v", err)
	}
	if err := mgr.startPlugin(ctx, "exclusive"); err != nil {
		t.Fatalf("exclusive 启动: %v", err)
	}

	mgr.mu.RLock()
	ha := mgr.hosts["co-a"]
	hx := mgr.hosts["exclusive"]
	mgr.mu.RUnlock()
	if ha == nil || ha.cohost == nil {
		t.Fatal("co-a 应该挂到共享宿主")
	}
	if hx == nil || hx.cohost != nil {
		t.Fatal("exclusive 应该保持独占进程")
	}

	// 调用路由：from 字段证明帧送到了对的 worker
	ra, err := ha.Call(ctx, "plugin.handle_message", map[string]any{"session": map[string]any{"text": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	var outA struct {
		From string `json:"from"`
	}
	_ = json.Unmarshal(ra, &outA)
	if outA.From != "a" {
		t.Fatalf("co-a 路由错误: %+v", outA)
	}

	// stopPlugin → startPlugin（重载路径）
	mgr.stopPlugin("co-a")
	mgr.mu.RLock()
	_, alive := mgr.hosts["co-a"]
	mgr.mu.RUnlock()
	if alive {
		t.Fatal("stopPlugin 后 co-a 应该从 hosts 移除")
	}
	if err := mgr.startPlugin(ctx, "co-a"); err != nil {
		t.Fatalf("co-a 重新启动: %v", err)
	}
	mgr.mu.RLock()
	ha = mgr.hosts["co-a"]
	mgr.mu.RUnlock()
	if _, err := ha.Call(ctx, "plugin.handle_message", map[string]any{"session": map[string]any{"text": "y"}}); err != nil {
		t.Fatal(err)
	}

	// exclusive 的 Kill/Stop 路径不受影响（能 Stop 不 panic）
	hx.Stop(context.Background())

	// 清理：StopAll 关掉共享宿主与独占进程
	mgr.StopAll(context.Background())
}

// 熔断：共享进程 10 分钟内死 3 次 → 降级为独占（cohostEnabled 变 false）。
func repoDirForTest() string {
	p, _ := filepath.Abs(filepath.Join("..", ".."))
	return p
}

func TestCohostCircuitBreaker(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir, filepath.Join(dir, "data"), filepath.Join(dir, "state.json"), registry.New(),
		perm.New("allow_all", nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.SDKDir = filepath.Join(repoDirForTest(), "sdk", "node")
	mgr.SetCohostEnabled(true)
	if !mgr.cohostOn.Load() {
		t.Fatal("初始应开启")
	}
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		dead := &SharedHost{done: make(chan struct{})}
		close(dead.done)
		mgr.sharedMu.Lock()
		mgr.sharedPools["test"] = dead
		mgr.sharedMu.Unlock()
		_, err := mgr.ensureSharedHost(ctx, "test", 6)
		if i < 3 && err != nil {
			t.Fatalf("第 %d 次死亡不该触发熔断: %v", i, err)
		}
		if i == 3 && err == nil {
			t.Fatal("第 3 次死亡应触发熔断并拒绝重建")
		}
	}
	if !mgr.CohostPoolDegraded("test") {
		t.Fatal("池应已熔断")
	}
	mgr.StopAll(context.Background())
}

func TestWorkerCrashesEvictPlugin(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir, filepath.Join(dir, "data"), filepath.Join(dir, "state.json"), registry.New(),
		perm.New("allow_all", nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	pool, err := mgr.Pools.Create("p", 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Pools.Assign("canary", pool.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		mgr.onCohostWorkerExit(pool.ID, "canary", 1)
	}
	_, ass, ok := mgr.Pools.PoolOf("canary")
	if !ok || ass.State != "evicted" {
		t.Fatalf("3 次崩溃后应逐出，得到 %+v %v", ass, ok)
	}
}

// canary：同池里一个插件连续崩 3 次只逐出自己，不把同池的 stable 一起踢掉。
func TestCohostCanaryEvictedNotWholePool(t *testing.T) {
	repo, _ := filepath.Abs(filepath.Join("..", ".."))
	sdkDir := filepath.Join(repo, "sdk", "node")
	dir := t.TempDir()

	writeCohostDemoPlugin(t, dir, "canary", `import { serve } from '../_sdk/qixin.mjs'
serve({
  async handle_message({ session }) {
    if (session.text === 'boom') {
      // 立刻退出 worker：比 setTimeout 更容易稳定复现崩溃
      process.exit(1)
    }
    return { handled: true, from: 'canary' }
  },
})
`)
	writeCohostDemoPlugin(t, dir, "stable", `import { serve } from '../_sdk/qixin.mjs'
serve({
  async handle_message() { return { handled: true, from: 'stable' } },
})
`)

	mgr := NewManager(dir, filepath.Join(dir, "data"), filepath.Join(dir, "state.json"), registry.New(),
		perm.New("allow_all", nil), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	mgr.SDKDir = sdkDir
	mgr.OnHostCall = func(ctx context.Context, man *Manifest, method string, params json.RawMessage) (any, *RPCError) {
		return map[string]any{}, nil
	}
	mgr.SetCohostEnabled(true)
	ctx := context.Background()
	t.Cleanup(func() { mgr.StopAll(context.Background()) })

	pool, err := mgr.Pools.Create("canary-pool", 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"canary", "stable"} {
		if err := mgr.Pools.Assign(id, pool.ID); err != nil {
			t.Fatal(err)
		}
		if err := mgr.startPlugin(ctx, id); err != nil {
			t.Fatalf("%s 启动: %v", id, err)
		}
	}

	mgr.mu.RLock()
	hc := mgr.hosts["canary"]
	hs := mgr.hosts["stable"]
	mgr.mu.RUnlock()
	if hc == nil || hc.cohost == nil || hs == nil || hs.cohost == nil {
		t.Fatal("canary/stable 应都在共享池")
	}

	// 触发 canary worker 崩溃 3 次；每次等 worker 自动重启后再打下一次
	for i := 0; i < 3; i++ {
		// worker 自杀时调用本身可能拿不到响应，这是预期，不当作失败
		_, _ = hc.Call(ctx, "plugin.handle_message", map[string]any{"session": map[string]any{"text": "boom"}})
		if i < 2 {
			readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := hc.cohost.WaitPluginReady(readyCtx, "canary"); err != nil {
				cancel()
				t.Fatalf("canary 第 %d 次崩溃后未恢复: %v", i+1, err)
			}
			cancel()
		}
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, ass, ok := mgr.Pools.PoolOf("canary"); ok && ass.State == "evicted" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, ass, ok := mgr.Pools.PoolOf("canary")
	if !ok || ass.State != "evicted" {
		t.Fatalf("canary 应被逐出，得到 %+v %v", ass, ok)
	}
	if _, _, ok := mgr.Pools.PoolOf("stable"); !ok {
		t.Fatal("stable 不应被一起逐出")
	}
	mgr.mu.RLock()
	hs2 := mgr.hosts["stable"]
	mgr.mu.RUnlock()
	if hs2 == nil || hs2.cohost == nil {
		t.Fatal("stable 应仍在共享池")
	}
}

func TestEvictedPluginDisabledAfterThreeCrashes(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir, filepath.Join(dir, "data"), filepath.Join(dir, "state.json"), registry.New(),
		perm.New("allow_all", nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.cohostEvicted["canary"] = true
	mgr.restarts["canary"] = 2
	h := NewHost(&Manifest{ID: "canary", Name: "canary"}, filepath.Join(dir, "canary"), filepath.Join(dir, "canary", "data"), mgr.Log)
	close(h.done)
	mgr.supervise(h)
	if got := mgr.StateOf("canary"); got != "disabled" {
		t.Fatalf("逐出后第 3 次独立崩溃应禁用，当前状态 %q", got)
	}
}

func TestEvictedStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	mk := func() *Manager {
		return NewManager(dir, filepath.Join(dir, "data"), filepath.Join(dir, "state.json"), registry.New(),
			perm.New("allow_all", nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	m1 := mk()
	pool, err := m1.Pools.Create("p", 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := m1.Pools.Assign("canary", pool.ID); err != nil {
		t.Fatal(err)
	}
	if err := m1.Pools.Evict("canary"); err != nil {
		t.Fatal(err)
	}
	m2 := mk()
	if !m2.cohostEvicted["canary"] {
		t.Fatal("逐出状态应跨重启保留")
	}
}
