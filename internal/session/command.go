package session

// 结构化命令：由宿主统一解析，插件直接拿结果。
//
// 以前插件要在 handle_message 里自己干这些：
//
//	const text = session.text
//	if (!text.startsWith('weather ')) return { handled: false }
//	const city = text.slice(8).trim()
//	if (!city) return { handled: false, reply: '用法：/weather <城市>' }
//
// 现在 manifest 里声明了参数，宿主就填好这些字段：
//
//	{"command": "weather.current", "args": {"city": "上海"}}
//
// **插件仍然可以自己解析**（看 session.text）——声明是可选增强，不是强制迁移。

// CommandMatch 描述一次命令匹配的结果。
type CommandMatch struct {
	// ID 是 manifest 里那条命令的 id（如 weather.current）
	ID string `json:"id"`
	// Name 是匹配到的命令词（usage 的第一个词，如 weather）
	Name string `json:"name"`
	// Args 是按占位符顺序解析出来的位置参数
	Args map[string]string `json:"args,omitempty"`
	// Raw 是命令词之后、没被解析的剩余文本
	Raw string `json:"raw,omitempty"`
	// Missing 是必填但没给全的参数名（插件可以据此回用法提示）
	Missing []string `json:"missing,omitempty"`
}

// Complete 表示所有必填参数都给了。
func (m *CommandMatch) Complete() bool { return len(m.Missing) == 0 }
