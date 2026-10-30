package plugin

import (
	"strings"

	"github.com/xin/onebot/internal/session"
)

// ParseCommand 按 manifest 里声明的 usage 和 args，把文本解析成结构化命令。
//
// 规则：
//   - usage 的第一个词是命令名，比如 "weather <城市>" → "weather"
//   - `<xx>` 是位置参数，按出现顺序从文本里取词
//   - 类型 `rest` 吃掉剩下的全部（可以带空格）
//   - 匹配不上返回 nil（插件自己处理）
//
// 例子：
//
//	usage = "weather <城市>"
//	text  = "weather 上海"
//	→ &CommandMatch{ID:"weather.current", Name:"weather", Args:{"城市":"上海"}}
func ParseCommand(man *Manifest, text string) *session.CommandMatch {
	if man == nil || text == "" {
		return nil
	}
	// 命令名取 usage 的第一个词（去掉前导斜杠已在加载时处理）
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	head := fields[0]
	for i := range man.Commands {
		c := &man.Commands[i]
		usage := strings.TrimSpace(c.Usage)
		if usage == "" {
			continue
		}
		// usage 的第一个词
		usageWord := usage
		if idx := strings.IndexAny(usage, " \t"); idx > 0 {
			usageWord = usage[:idx]
		}
		// 匹配命令词：usage 的第一个词，或任一别名。
		// 别名以前只能让插件收到消息，现在也能生成结构化参数。
		matched := ""
		if equalCommand(head, usageWord) {
			matched = usageWord
		} else {
			for _, a := range c.Aliases {
				if a != "" && equalCommand(head, a) {
					matched = a
					break
				}
			}
		}
		if matched == "" {
			continue
		}

		m := &session.CommandMatch{
			ID:   c.ID,
			Name: matched, // 实际匹配到的词（可能是别名）
			Args: map[string]string{},
		}

		// 没有声明 args 就不解析——插件照旧自己处理
		if len(c.Args) == 0 {
			m.Raw = strings.TrimSpace(strings.TrimPrefix(text, head))
			return m
		}

		// 按 usage 里 <占位符> 的顺序取
		placeholders := extractPlaceholders(usage)
		rest := strings.TrimSpace(strings.TrimPrefix(text, head))

		for idx, name := range placeholders {
			spec, declared := c.Args[name]
			if !declared {
				// usage 里有但 args 没声明：按 string 处理
				spec = ArgSpec{Type: "string"}
			}
			if spec.Type == "rest" {
				// rest 吃掉剩余全部
				if rest != "" {
					m.Args[name] = rest
					rest = ""
				} else if spec.Required {
					m.Missing = append(m.Missing, name)
				}
				continue
			}

			// 普通参数：取一个词
			parts := strings.Fields(rest)
			if len(parts) == 0 {
				if spec.Required {
					m.Missing = append(m.Missing, name)
				}
				continue
			}
			m.Args[name] = parts[0]
			rest = strings.TrimSpace(strings.TrimPrefix(rest, parts[0]))
			_ = idx
		}
		// 剩下没被占位符吃掉的文本
		if rest != "" {
			m.Raw = rest
		}
		return m
	}
	return nil
}

// equalCommand 比较命令词：大小写不敏感，允许别名。
func equalCommand(got, want string) bool {
	return strings.EqualFold(got, want)
}

// extractPlaceholders 从 usage 里抠出 <xx> 的名字，按顺序。
func extractPlaceholders(usage string) []string {
	var out []string
	i := 0
	for i < len(usage) {
		start := strings.IndexByte(usage[i:], '<')
		if start < 0 {
			break
		}
		start += i
		end := strings.IndexByte(usage[start:], '>')
		if end < 0 {
			break
		}
		end += start
		name := strings.TrimSpace(usage[start+1 : end])
		if name != "" {
			out = append(out, name)
		}
		i = end + 1
	}
	return out
}
