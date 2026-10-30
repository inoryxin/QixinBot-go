package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runNew 生成一个最小可运行的插件骨架。
// 目标是"能直接 pack + 启用 + 收到消息"，而不是展示所有能力。
func runNew(args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "用法: qixinpack new <插件id> [-name 中文名] [-dir 输出父目录] [-force]")
		os.Exit(2)
	}

	id := args[0]
	name := id
	parent := "."
	force := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-name", "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "-dir", "--dir":
			if i+1 < len(args) {
				parent = args[i+1]
				i++
			}
		case "-force", "--force":
			force = true
		}
	}

	dir := filepath.Join(parent, id)
	if _, err := os.Stat(dir); err == nil && !force {
		fmt.Fprintf(os.Stderr, "目录已存在: %s（加 -force 覆盖）\n", dir)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		fail("建目录失败", err)
	}

	files := map[string]string{
		"manifest.toml":      manifestTemplate(id, name),
		"app/index.js":       appTemplate(id),
		"web/panel.html":     panelTemplate(),
		"README.md":          readmeTemplate(id, name),
	}

	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fail("建目录失败", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			fail("写文件失败", err)
		}
	}

	fmt.Printf("已生成插件骨架: %s\n\n", dir)
	fmt.Println("下一步：")
	fmt.Printf("  1. 改 manifest.toml 里的命令和说明\n")
	fmt.Printf("  2. 写 app/index.js\n")
	fmt.Printf("  3. .\\bin\\qixinpack.exe pack %s -out plugins/%s.anzu\n", dir, id)
	fmt.Printf("  4. 放到 plugins/ 目录，热重载会自动装好（首次需在 WebUI 里启用）\n")
}

func manifestTemplate(id, name string) string {
	return fmt.Sprintf(`id = "%s"
name = "%s"
version = "0.1.0"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
description = "一句话说明这个插件做什么"

# 权限：需要发消息就保留，只是回复当前会话可以不写
permissions = ["onebot.write"]

# 兜底插件（AI 聊天类）：永远排在普通插件之后接消息
# last = true

# 分发优先级：大的先收到消息
# priority = 0

# 并发：session = 同一会话串行、不同会话并发
# concurrency = "session"

# 运行环境要求（不写会自动从 exec[0] 推断）
# [runtime]
# kind = "node"
# version = ">=18"

[[commands]]
id = "%s.hello"
usage = "%s hello"
description = "测试命令：回一句问候"

# 工具（给 AI 调用的能力），需要时再打开
# [[tools]]
# name = "%s_echo"
# description = "把输入原样返回"
#
# [tools.parameters]
# type = "object"
# required = ["text"]
#
# [tools.parameters.properties.text]
# type = "string"
`, id, name, id, id, id)
}

func appTemplate(id string) string {
	return fmt.Sprintf(`// %s 插件入口。
// 协议细节见 PLUGIN.md，接口清单见 API.md。
import { serve, host, chatOf } from '../_sdk/qixin.mjs'

let settings = {}

serve({
  // 握手：拿到配置和插件自己的数据目录
  async initialize({ plugin_id, settings: initial }) {
    settings = initial || {}
    host.log('info', %q + ' 已启动')
    return { api_version: '1.0' }
  },

  // 设置页保存后会推过来
  async set_settings({ values }) {
    settings = { ...settings, ...values }
    return { ok: true }
  },

  // 用户消息
  async handle_message({ session }) {
    const text = (session.text || '').trim()

    if (text === '%s hello') {
      await host.reply(chatOf(session), '你好，我是 %s')
      return { handled: true }   // 我处理了，不再往下传
    }

    return { handled: false }    // 不是我的
  },
})
`, id, id, id, id)
}

func panelTemplate() string {
	return `<!-- 可选：插件详情页里的扩展片段。
     只有需要额外交互时才用，普通设置用 manifest 的 [[settings]] 就够了。
     删掉这个文件也不会影响插件运行。 -->
<div style="margin-top:12px">
  <p class="muted">这是 web/panel.html，插件自定义区域。可以放按钮、图表、说明。</p>
</div>
`
}

func readmeTemplate(id, name string) string {
	return "# " + name + "（" + id + "）\n\n" +
		"一句话说明这个插件做什么。\n\n" +
		"## 命令\n\n" +
		"- `/" + id + " hello` — 回一句问候\n\n" +
		"## 设置\n\n" +
		"暂时没有。用 manifest 的 `[[settings]]` 声明后，WebUI 会自动渲染表单。\n\n" +
		"## 开发\n\n" +
		"```powershell\n" +
		".\\bin\\qixinpack.exe pack . -out ..\\" + id + ".anzu\n" +
		"```\n"
}
