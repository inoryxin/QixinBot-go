package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// runDev 是 `qixinpack dev <插件目录>`：
//
//	建一个临时工作区 → 打包插件 → 起假的 OneBot 协议端 → 起框架 → 打印控制台地址
//
// Ctrl+C 一起退出，临时目录自动删。
//
// 这是插件作者的日常命令：改完代码重跑一遍，在 WebUI 的「虚拟群」里发消息看效果，
// 全程不碰真实 QQ。
func runDev(args []string) {
	if len(args) == 0 {
		fmt.Println(`用法: qixinpack dev <插件目录> [选项]

  起一个本地开发环境：假协议端 + 框架 + 你的插件。
  Ctrl+C 退出，临时文件自动清理。

选项:
  -web  <端口>   控制台端口（默认 7801）
  -ws   <端口>   假协议端端口（默认 3001）
  -watch         监听源码变动，保存后自动重新打包并重载
  -runtime <包>  使用指定运行时包（可重复，Node/Python 各一个）
  -keep          退出后保留临时目录（排查用）`)
		os.Exit(1)
	}

	pluginDir := args[0]
	webPort := 7801
	wsPort := 3001
	keep := false
	watch := false
	var runtimePacks []string
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
		case "-watch":
			watch = true
		case "-runtime":
			if i+1 < len(args) {
				i++
				runtimePacks = append(runtimePacks, args[i])
			}
		}
	}

	absPlugin, err := filepath.Abs(pluginDir)
	if err != nil {
		fail("解析插件路径失败: %v", err)
	}
	manPath := filepath.Join(absPlugin, "manifest.toml")
	if _, err := os.Stat(manPath); err != nil {
		fail("找不到 %s —— 请在插件目录里运行，或者把路径指对", manPath)
	}

	// 插件 id 从 manifest 里读（重载接口要用）
	pluginID := readPluginID(manPath)

	// 1. 工作区：建在当前目录下，不用系统临时目录
	// （某些环境 %TEMP% 没写权限，建在工作目录最稳）
	work, err := os.MkdirTemp(".", ".dev-*")
	if err != nil {
		fail("建开发目录失败: %v", err)
	}
	if abs, err := filepath.Abs(work); err == nil {
		work = abs
	}
	if !keep {
		defer os.RemoveAll(work)
	}
	pluginsDir := filepath.Join(work, "plugins")
	dataDir := filepath.Join(work, "data")
	_ = os.MkdirAll(pluginsDir, 0o755)
	_ = os.MkdirAll(dataDir, 0o755)

	// 1.5 先装运行时包：dev 用和生产一致的 Node/Python + 共享依赖
	if err := installRuntimePackages(work, runtimePacks); err != nil {
		fail("安装运行时失败: %v", err)
	}

	// 2. 打包插件到工作区
	pkgName := filepath.Base(absPlugin) + ".anzu"
	outPkg := filepath.Join(pluginsDir, pkgName)
	fmt.Printf("==> 打包 %s\n", filepath.Base(absPlugin))
	runPackTo(absPlugin, outPkg)

	// 3. 找框架和 mock 二进制
	botBin := findBinary("qixinbot")
	mockBin := findBinary("mock-onebot")
	if botBin == "" {
		fail("找不到 qixinbot —— 先跑 tools/build.ps1，或者把 bin 目录加进 PATH")
	}
	if mockBin == "" {
		fail("找不到 mock-onebot —— 先跑 tools/build.ps1")
	}

	// 4. 写临时 config
	cfgPath := filepath.Join(work, "config.toml")
	// SDK 在框架根目录下；dev 的工作区是临时目录，得用绝对路径指过去
	sdkPath := ""
	if p := findSDK(); p != "" {
		sdkPath = p
	} else {
		fail("找不到 sdk/node/qixin.mjs —— 请在框架仓库根目录下运行 qixinpack dev")
	}

	cfg := fmt.Sprintf(`# qixinpack dev 自动生成，别手改
[owner]
users = [10001]

[command]
prefixes = ["/"]

[plugin]
dir = "plugins"
runtime_dir = "data/plugins"
state_dir = "data"
hot_reload = true
auto_enable = true
allow_unsigned = true
sdk_dir = %q

[web]
enable = true
addr = "127.0.0.1:%d"
username = "admin"
password = "dev"

[log]
level = "debug"

[storage]
driver = "sqlite"

[[accounts]]
id = "main"
ws_url = "ws://127.0.0.1:%d"
access_token = "dev"
`, sdkPath, webPort, wsPort)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		fail("写 config 失败: %v", err)
	}

	// 5. 起假协议端
	fmt.Printf("==> 启动假协议端 :%d\n", wsPort)
	mockCmd := exec.Command(mockBin, "-addr", fmt.Sprintf("127.0.0.1:%d", wsPort))
	mockCmd.Dir = work
	mockCmd.Stdout = os.Stdout
	mockCmd.Stderr = os.Stderr
	setProcGroup(mockCmd)
	if err := mockCmd.Start(); err != nil {
		fail("启动 mock 失败: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	// 6. 起框架
	fmt.Printf("==> 启动框架，控制台 http://127.0.0.1:%d （admin / dev）\n", webPort)
	botCmd := exec.Command(botBin, "-config", cfgPath)
	botCmd.Dir = work
	botCmd.Stdout = os.Stdout
	botCmd.Stderr = os.Stderr
	setProcGroup(botCmd)
	if err := botCmd.Start(); err != nil {
		_ = mockCmd.Process.Kill()
		fail("启动框架失败: %v", err)
	}

	// 7. 等控制台起来，打印提示
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", webPort), 300*time.Millisecond); err == nil {
				_ = conn.Close()
				fmt.Printf("\n✅ 就绪\n")
				fmt.Printf("   控制台   http://127.0.0.1:%d  (admin / dev)\n", webPort)
				fmt.Printf("   虚拟群   控制台 → 聊天 → 🧪 虚拟群 → 发消息测插件\n")
				fmt.Printf("   插件包   %s\n", outPkg)
				fmt.Printf("\n   Ctrl+C 退出\n\n")
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Println("⚠️  控制台 15 秒内没起来，看上面的日志")
	}()

	// 8. 源码监听（可选）
	stopWatch := make(chan struct{})
	if watch {
		w := &watcher{
			pluginDir: absPlugin,
			pkgPath:   outPkg,
			pluginID:  pluginID,
			webAddr:   fmt.Sprintf("127.0.0.1:%d", webPort),
			skip:      defaultSkip(),
		}
		go w.watchLoop(stopWatch)
	}

	// 9. Ctrl+C 一起退
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	close(stopWatch)
	fmt.Println("\n==> 退出中…")
	// 连插件的子进程一起收掉：只杀框架会让插件进程变孤儿
	killTree(botCmd)
	killTree(mockCmd)
	if keep {
		fmt.Printf("临时目录保留在: %s\n", work)
	}
}

// findBinary 找同目录的兄弟二进制（qixinbot / mock-onebot）。
//
// 查找顺序（重要：从插件目录跑 `..\bin\qixinpack.exe dev .` 时，
// 当前目录是插件目录，里面没有 bin/，所以必须先看 qixinpack 自己那层）：
//   1. qixinpack.exe 所在目录
//   2. 它的上一级的 bin/
//   3. 当前目录的 bin/（从框架根目录跑）
//   4. 当前目录
//   5. PATH
func findBinary(name string) string {
	names := []string{name}
	if runtime.GOOS == "windows" {
		names = []string{name + ".exe", name}
	}

	var dirs []string
	if self, err := os.Executable(); err == nil {
		selfDir := filepath.Dir(self)
		dirs = append(dirs,
			selfDir,
			filepath.Join(selfDir, "..", "bin"),
			filepath.Join(selfDir, "..", "..", "bin"),
		)
	}
	dirs = append(dirs,
		filepath.Join("bin"),
		filepath.Join("bin", runtime.GOOS),
		".",
	)
	for _, dir := range dirs {
		for _, n := range names {
			p := filepath.Join(dir, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				abs, _ := filepath.Abs(p)
				return abs
			}
		}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// runPackTo 把插件目录打成 .anzu（复用 pack 的逻辑）。
func runPackTo(dir, out string) {
	runPack([]string{dir, "-out", out})
}

// findSDK 找 sdk/node 目录（里面是 qixin.mjs）。
//
// 查找顺序和 findBinary 一样：先看 qixinpack 自己那层，
// 这样从插件目录跑 `..\bin\qixinpack.exe dev .` 也能找到。
func findSDK() string {
	var roots []string
	if self, err := os.Executable(); err == nil {
		selfDir := filepath.Dir(self)
		roots = append(roots,
			filepath.Join(selfDir, ".."),
			filepath.Join(selfDir, "..", ".."),
			selfDir,
		)
	}
	roots = append(roots, ".")
	for _, root := range roots {
		p := filepath.Join(root, "sdk", "node")
		if st, err := os.Stat(filepath.Join(p, "qixin.mjs")); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}
