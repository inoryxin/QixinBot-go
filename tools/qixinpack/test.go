package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// testCase 是一个测试场景。
type testCase struct {
	name     string // 场景名
	text     string // 发什么
	identity string // owner | admin | member
	expect   string // handled | ignored（期望插件认领还是不认领）
}

// testResult 是一条结果。
type testResult struct {
	testCase
	ok      bool
	handled bool
	plugin  string
	replies []string
	err     string
}

// runTest 是 `qixinpack test <插件目录>`：
//
//	起一个和 dev 一样的环境，按 manifest 里声明的命令逐条发消息，
//	检查插件有没有正确认领、有没有回复。
//
// 它不能替代人工点一遍，但能保证"改完之后命令还在、还认领"。
func runTest(args []string) int {
	if len(args) == 0 {
		fmt.Println(`用法: qixinpack test <插件目录> [选项]

  起临时环境，按 manifest 声明的命令逐条发消息，检查插件是否正常响应。

选项:
  -web  <端口>   控制台端口（默认 7811，避开 dev 的 7801）
  -ws   <端口>   假协议端端口（默认 3011）
  -keep          保留临时目录`)
		os.Exit(1)
	}

	pluginDir := args[0]
	webPort, wsPort, keep := 7811, 3011, false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-web":
			if i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &webPort)
			}
		case "-ws":
			if i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &wsPort)
			}
		case "-keep":
			keep = true
		}
	}

	absPlugin, err := filepath.Abs(pluginDir)
	if err != nil {
		fail("解析路径失败: %v", err)
	}
	manPath := filepath.Join(absPlugin, "manifest.toml")
	if _, err := os.Stat(manPath); err != nil {
		fail("找不到 %s", manPath)
	}

	pluginID := readPluginID(manPath)
	commands := readCommands(manPath)

	fmt.Printf("==> 测试插件 %s\n", pluginID)
	if len(commands) == 0 {
		fmt.Println("  ⚠️  manifest 里没有 [[commands]]，只能测通用消息")
	}

	// 1. 建环境
	work, err := os.MkdirTemp(".", ".test-*")
	if err != nil {
		fail("建测试目录失败: %v", err)
	}
	if abs, err := filepath.Abs(work); err == nil {
		work = abs
	}
	if !keep {
		defer os.RemoveAll(work)
	}
	pluginsDir := filepath.Join(work, "plugins")
	_ = os.MkdirAll(pluginsDir, 0o755)

	pkgPath := filepath.Join(pluginsDir, pluginID+".anzu")
	runPackTo(absPlugin, pkgPath)

	// 2. 起环境
	cleanup, base := startTestEnv(work, webPort, wsPort)
	defer cleanup()

	// 3. 登录
	client, err := loginTest(base)
	if err != nil {
		// 不能用 fail() 硬退：它直接 os.Exit，defer 里的
		// killTree / 删临时目录全都跳过，qixinbot 就成了孤儿进程
		fmt.Printf("登录失败: %v\n", err)
		return 1
	}

	// 4. 造场景
	cases := buildCases(commands)

	// 5. 逐条跑
	var results []testResult
	for _, tc := range cases {
		r := runOne(client, base, tc)
		results = append(results, r)
	}

	// 6. 报告（返回失败数，由调用方在 defer 全部跑完后退出）
	return report(results)
}

// buildCases 按声明的命令生成测试场景。
func buildCases(commands []string) []testCase {
	var cases []testCase

	for _, usage := range commands {
		// usage 形如 "weather <city>"，把 <> 换成示例值。
		// 前缀不能省——多数插件配了 require_prefix = true，
		// 不带前缀会被门禁直接拒掉，看着像"插件没响应"。
		msg := "/" + fillUsage(usage)
		cases = append(cases, testCase{
			name:     "命令 " + usage,
			text:     msg,
			identity: "owner",
			expect:   "handled",
		})
	}

	// 通用场景
	cases = append(cases,
		testCase{name: "无意义文本", text: "随便说点什么测试一下", identity: "owner", expect: "ignored"},
		testCase{name: "空消息", text: " ", identity: "owner", expect: "ignored"},
	)
	return cases
}

// fillUsage 把 "weather <city>" 变成 "weather 示例"，用来测试命令能触发。
func fillUsage(usage string) string {
	out := usage
	for {
		start := strings.IndexByte(out, '<')
		if start < 0 {
			break
		}
		end := strings.IndexByte(out[start:], '>')
		if end < 0 {
			break
		}
		end += start
		out = out[:start] + "示例" + out[end+1:]
	}
	return strings.TrimSpace(out)
}

// runOne 跑一个场景。
func runOne(client *http.Client, base string, tc testCase) testResult {
	res := testResult{testCase: tc}

	form := url.Values{
		"text":      {tc.text},
		"chat_type": {"group"},
		"chat_id":   {"0"},
		"identity":  {tc.identity},
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/chat/inject", strings.NewReader(form.Encode()))
	if err != nil {
		res.err = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token := csrfToken(client, base); token != "" {
		req.Header.Set("X-CSRF-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		res.err = err.Error()
		return res
	}
	defer resp.Body.Close()

	var body struct {
		OK      bool   `json:"ok"`
		Handled bool   `json:"handled"`
		Plugin  string `json:"plugin"`
		Replies []struct {
			Segments []struct {
				Type string         `json:"type"`
				Data map[string]any `json:"data"`
			} `json:"segments"`
		} `json:"replies"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		res.err = err.Error()
		return res
	}
	res.handled = body.Handled
	res.plugin = body.Plugin
	for _, rep := range body.Replies {
		var parts []string
		for _, seg := range rep.Segments {
			if seg.Type == "text" {
				if s, ok := seg.Data["text"].(string); ok {
					parts = append(parts, s)
				}
			} else {
				parts = append(parts, "["+seg.Type+"]")
			}
		}
		res.replies = append(res.replies, strings.Join(parts, ""))
	}

	switch tc.expect {
	case "handled":
		res.ok = body.Handled
	case "ignored":
		res.ok = !body.Handled
	}
	return res
}

// report 打印结果。
func report(results []testResult) int {
	fmt.Printf("\n===== 测试结果 =====\n")
	pass, fail := 0, 0
	for _, r := range results {
		mark := "✅"
		if !r.ok {
			mark = "❌"
			fail++
		} else {
			pass++
		}
		fmt.Printf("%s %s\n", mark, r.name)
		fmt.Printf("   发送: %q（身份 %s）\n", r.text, r.identity)
		if r.err != "" {
			fmt.Printf("   错误: %s\n", r.err)
			continue
		}
		if r.handled {
			fmt.Printf("   认领: %s\n", r.plugin)
		} else {
			fmt.Printf("   认领: 无\n")
		}
		for i, rep := range r.replies {
			preview := rep
			if len(preview) > 120 {
				preview = preview[:120] + "…"
			}
			fmt.Printf("   回复%d: %s\n", i+1, strings.ReplaceAll(preview, "\n", " / "))
		}
		if !r.ok {
			if r.expect == "handled" && !r.handled {
				fmt.Printf("   ⚠️  期望插件认领，但没人处理——检查 usage 和 manifest\n")
			}
			if r.expect == "ignored" && r.handled {
				fmt.Printf("   ⚠️  期望忽略，但被 %s 认领了——可能匹配太宽\n", r.plugin)
			}
		}
		fmt.Println()
	}
	fmt.Printf("===== %d 通过 / %d 失败 =====\n", pass, fail)
	// 返回失败数而不是在这里 os.Exit：
	// 直接退出会跳过 runTest 的 defer（killTree / 删临时目录），
	// qixinbot 和 mock-onebot 就成了孤儿，下次测试报"端口被占用"
	return fail
}

// startTestEnv 起 mock + 框架，返回清理函数和控制台地址。
func startTestEnv(work string, webPort, wsPort int) (func(), string) {
	botBin := findBinary("qixinbot")
	mockBin := findBinary("mock-onebot")
	if botBin == "" || mockBin == "" {
		fail("找不到 qixinbot / mock-onebot，先跑 tools/build.ps1")
	}
	sdkPath := findSDK()
	if sdkPath == "" {
		fail("找不到 sdk/node")
	}

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
password = "test"
[log]
level = "warn"
[storage]
driver = "sqlite"
[[accounts]]
id = "main"
ws_url = "ws://127.0.0.1:%d"
access_token = "test"
`, sdkPath, webPort, wsPort)
	if err := os.WriteFile(filepath.Join(work, "config.toml"), []byte(cfg), 0o644); err != nil {
		fail("写 config 失败: %v", err)
	}

	mockCmd := exec.Command(mockBin, "-addr", fmt.Sprintf("127.0.0.1:%d", wsPort))
	mockCmd.Dir = work
	mockCmd.Stdout = os.Stdout
	mockCmd.Stderr = os.Stderr
	setProcGroup(mockCmd)
	if err := mockCmd.Start(); err != nil {
		fail("启动 mock 失败: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	botCmd := exec.Command(botBin, "-config", "config.toml")
	botCmd.Dir = work
	botCmd.Stdout = os.Stdout
	botCmd.Stderr = os.Stderr
	setProcGroup(botCmd)
	if err := botCmd.Start(); err != nil {
		killTree(mockCmd)
		fail("启动框架失败: %v", err)
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", webPort)
	// 等控制台
	deadline := time.Now().Add(20 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !ready {
		killTree(botCmd)
		killTree(mockCmd)
		fail("控制台 20 秒内没起来")
	}
	// 再等插件加载完
	time.Sleep(2 * time.Second)

	return func() {
		killTree(botCmd)
		killTree(mockCmd)
	}, base
}

// csrfToken 从登录后的 Cookie 里取 CSRF token（写操作要带 X-CSRF-Token）。
func csrfToken(client *http.Client, base string) string {
	u, err := url.Parse(base)
	if err != nil || client.Jar == nil {
		return ""
	}
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == "qixin_csrf" {
			return c.Value
		}
	}
	return ""
}

// loginTest 登录测试控制台。
func loginTest(base string) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	form := url.Values{"username": {"admin"}, "password": {"test"}}
	resp, err := client.PostForm(base+"/login", form)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return client, nil
}

// readCommands 从 manifest 里抠出所有 usage。
func readCommands(manifestPath string) []string {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "usage = ") || strings.HasPrefix(line, "usage=") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "usage"))
			v = strings.TrimSpace(strings.TrimPrefix(v, "="))
			v = strings.Trim(v, "\"'")
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}
