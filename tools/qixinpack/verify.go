package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// verifyCase 是一个端到端验证场景。
type verifyCase struct {
	Name string
	Run  func(ctx *verifyCtx) (bool, string)
}

// verifyCtx 携带运行中的环境。
type verifyCtx struct {
	work    string
	webPort int
	wsPort  int
	bot     *exec.Cmd
	mock    *exec.Cmd
	base    string
	logPath string
}

// runVerify 是 `qixinpack verify`：
//
// 走**真实 WebSocket**（不是控制台注入），验证那些只有真实链路才暴露的问题：
//   - host.ask 等待期间会不会堵住其它群
//   - 两个群同时调同一个插件会不会并行
//
// 这比 dev 更严格：dev 只能看"命令有没有响应"，verify 能看"时序对不对"。
func runVerify(args []string) {
	only := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-ask":
			only = "ask"
		case "-concurrency":
			only = "concurrency"
		case "-all", "":
			only = ""
		}
	}

	cases := []verifyCase{
		{Name: "ask 等待时不堵其它群", Run: verifyAskNonBlocking},
		{Name: "两个群同时调同一插件（session 模式）", Run: verifyConcurrentGroups},
	}

	if only == "ask" {
		cases = cases[:1]
	} else if only == "concurrency" {
		cases = cases[1:]
	}

	fmt.Println("==> 端到端验证（走真实 WebSocket）")
	pass, fail := 0, 0
	for _, c := range cases {
		fmt.Printf("\n[%s]\n", c.Name)
		ctx, err := startVerifyEnv()
		if err != nil {
			fmt.Printf("  ❌ 环境启动失败: %v\n", err)
			fail++
			continue
		}
		ok, detail := c.Run(ctx)
		ctx.Stop(!ok)
		if ok {
			fmt.Printf("  ✅ %s\n", detail)
			pass++
		} else {
			fmt.Printf("  ❌ %s\n", detail)
			if data, err := os.ReadFile(ctx.logPath); err == nil {
				fmt.Println("  --- 日志末尾 ---")
				lines := strings.Split(string(data), "\n")
				start := len(lines) - 20
				if start < 0 {
					start = 0
				}
				for _, l := range lines[start:] {
					if strings.TrimSpace(l) != "" {
						fmt.Println("  " + l)
					}
				}
			}
			fail++
		}
	}

	fmt.Printf("\n===== %d 通过 / %d 失败 =====\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

// StartEnv 建临时环境（含测试插件）并启动 mock + 框架。
func startVerifyEnv() (*verifyCtx, error) {
	work, err := os.MkdirTemp(".", ".verify-*")
	if err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(work); err == nil {
		work = abs
	}
	pluginsDir := filepath.Join(work, "plugins")
	_ = os.MkdirAll(pluginsDir, 0o755)

	botBin := findBinary("qixinbot")
	mockBin := findBinary("mock-onebot")
	if botBin == "" || mockBin == "" {
		return nil, fmt.Errorf("找不到 qixinbot / mock-onebot，先跑 tools/build.ps1")
	}
	sdkPath := findSDK()
	if sdkPath == "" {
		return nil, fmt.Errorf("找不到 sdk/node")
	}

	// 造测试插件：一个会 host.ask，一个慢响应
	if err := writeVerifyPlugins(work); err != nil {
		return nil, err
	}

	webPort := 7821
	wsPort := 3021

	cfg := fmt.Sprintf(`[owner]
users = [10001]
[command]
prefixes = ["/"]
[plugin]
dir = "plugins"
runtime_dir = "data/plugins"
state_dir = "data"
hot_reload = false
auto_enable = true
allow_unsigned = true
sdk_dir = %q
[web]
enable = true
addr = "127.0.0.1:%d"
username = "admin"
password = "verify"
[log]
level = "info"
[storage]
driver = "sqlite"
[[accounts]]
id = "main"
ws_url = "ws://127.0.0.1:%d"
access_token = ""
`, sdkPath, webPort, wsPort)
	if err := os.WriteFile(filepath.Join(work, "config.toml"), []byte(cfg), 0o644); err != nil {
		return nil, err
	}

	mockCmd := exec.Command(mockBin, "-addr", fmt.Sprintf("127.0.0.1:%d", wsPort))
	mockCmd.Dir = work
	mockCmd.Stdout = io.Discard
	mockCmd.Stderr = io.Discard
	setProcGroup(mockCmd)
	if err := mockCmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 mock 失败: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	logPath := filepath.Join(work, "bot.log")
	logFile, _ := os.Create(logPath)
	botCmd := exec.Command(botBin, "-config", "config.toml")
	botCmd.Dir = work
	botCmd.Stdout = logFile
	botCmd.Stderr = logFile
	setProcGroup(botCmd)
	if err := botCmd.Start(); err != nil {
		killTree(mockCmd)
		return nil, fmt.Errorf("启动框架失败: %w", err)
	}

	ctx := &verifyCtx{
		work: work, webPort: webPort, wsPort: wsPort,
		bot: botCmd, mock: mockCmd,
		base:    fmt.Sprintf("http://127.0.0.1:%d", wsPort),
		logPath: logPath,
	}

	// 等控制台就绪
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", webPort))
		if err == nil {
			resp.Body.Close()
			time.Sleep(2 * time.Second) // 等插件加载
			return ctx, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	ctx.Stop(false)
	return nil, fmt.Errorf("框架 25 秒内没起来，看 %s", logPath)
}

func (c *verifyCtx) Stop(keep bool) {
	killTree(c.bot)
	killTree(c.mock)
	if !keep {
		_ = os.RemoveAll(c.work)
	} else {
		fmt.Printf("  （日志保留在 %s）\n", c.logPath)
	}
}

// push 让 mock 推一条消息。
func (c *verifyCtx) push(groupID, userID int64, text string) error {
	u := fmt.Sprintf("%s/push?text=%s&group=%d&user=%d",
		c.base, url.QueryEscape(text), groupID, userID)
	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// waitLog 在日志里等一个片段出现，返回出现时间。
func (c *verifyCtx) waitLog(fragment string, timeout time.Duration) (time.Time, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(c.logPath)
		if strings.Contains(string(data), fragment) {
			return time.Now(), true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return time.Time{}, false
}

// logCount 数日志里某个片段出现几次。
func (c *verifyCtx) logCount(fragment string) int {
	data, _ := os.ReadFile(c.logPath)
	return strings.Count(string(data), fragment)
}

// writeVerifyPlugins 生成两个测试插件。
func writeVerifyPlugins(work string) error {
	// 1. ask 插件：/bind 等回答，/vping 秒回
	askDir := filepath.Join(work, "plugins", "verify-ask.anzu")
	extra := `concurrency = "session"

[[commands]]
id = "verifyask.bind"
usage = "bind"

[[commands]]
id = "verifyask.ping"
usage = "vping"
`
	code := `import { serve, host, chatOf } from '../_sdk/qixin.mjs'
serve({
  async initialize() { return { api_version: '1.0' } },
  async handle_message({ session }) {
    const t = (session.text ?? '').trim()
    if (t === 'vping') {
      // 标记必须在 reply **之后**：先标记的话，
      // 即使发送卡住验证也会通过（假阳性）。
      // 放在这里能证明"整段处理走完，包括回复发出去了"。
      await host.reply(chatOf(session), 'pong')
      host.log('info', 'VERIFY_VPING ' + session.chat_id)
      return { handled: true }
    }
    if (t !== 'bind') return { handled: false }
    const chat = chatOf(session)
    chat.user_id = session.user_id
    host.log('info', 'VERIFY_ASK_START')
    const ans = await host.ask(chat, '请输入名字：', { timeout_sec: 20 })
    host.log('info', 'VERIFY_ASK_END ' + (ans || 'TIMEOUT'))
    return { handled: true }
  },
})
`
	if err := buildPluginZip(askDir, "verify-ask", "交互验证", extra, code); err != nil {
		return err
	}
	_ = askDir
	return nil
}

// buildPluginZip 造一个 .anzu（zip）文件。
func buildPluginZip(outPath, id, name, extraManifest, code string) error {
	manifest := fmt.Sprintf(`id = %q
name = %q
version = "1.0.0"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
description = "qixinpack verify 临时插件"

%s
[runtime]
kind = "node"
`, id, name, extraManifest)
	return writeZip(outPath, map[string]string{
		"manifest.toml": manifest,
		"app/index.js":  code,
	})
}

var _ = json.Marshal
