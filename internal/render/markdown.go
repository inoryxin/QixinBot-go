// Package render 把插件产出的文本处理成 QQ 能正常显示的样子。
package render

import (
	"regexp"
	"strings"
)

var (
	// ```lang\ncode\n``` 整个代码块
	reCodeBlock = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\\n?(.*?)```")
	// 行内 `code`
	reInlineCode = regexp.MustCompile("`([^`\\n]+)`")
	// 行首标题 ###
	reHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.*)$`)
	// 粗体 **x** / __x__
	reBold = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	// 链接 [文字](url)
	reLink = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\n]+)\)`)
	// 列表项
	reList = regexp.MustCompile(`(?m)^(\s*)[-*+]\s+`)
	// 引用
	reQuote = regexp.MustCompile(`(?m)^\s*>\s?`)
)

// ToPlain 把常见 Markdown 转成纯文本。
// 设计原则：只处理"成对且明确"的语法，宁可漏转也不要误伤。
// 比如 2*3*4 不会被当成斜体，因为斜体要求标记两侧是文字。
func ToPlain(s string) string {
	if s == "" || !looksLikeMarkdown(s) {
		return s
	}

	// 代码块：去掉围栏，保留内容（缩进两格避免被当成正文）
	s = reCodeBlock.ReplaceAllStringFunc(s, func(m string) string {
		inner := reCodeBlock.FindStringSubmatch(m)
		if len(inner) < 2 {
			return m
		}
		lines := strings.Split(strings.Trim(inner[1], "\n"), "\n")
		for i, l := range lines {
			lines[i] = "  " + l
		}
		return strings.Join(lines, "\n")
	})

	s = reInlineCode.ReplaceAllString(s, "$1")
	s = reHeading.ReplaceAllString(s, "$1")
	s = reLink.ReplaceAllString(s, "$1")
	s = stripHorizontalRules(s)

	// 粗体：**x** 或 __x__
	s = reBold.ReplaceAllStringFunc(s, func(m string) string {
		sub := reBold.FindStringSubmatch(m)
		if len(sub) == 3 {
			if sub[1] != "" {
				return sub[1]
			}
			return sub[2]
		}
		return m
	})

	s = stripItalic(s)
	s = reList.ReplaceAllString(s, "$1• ")
	s = reQuote.ReplaceAllString(s, "")

	// 收尾：多余空行压成一个
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// stripHorizontalRules 删掉 --- / *** / ___ 这样的分隔线。
// Go 的 regexp 不支持反向引用，所以手写。
func stripHorizontalRules(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if len(t) >= 3 && (allSame(t, '-') || allSame(t, '*') || allSame(t, '_')) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func allSame(s string, r rune) bool {
	for _, c := range s {
		if c != r {
			return false
		}
	}
	return true
}

// looksLikeMarkdown 先做一个廉价判断，避免对纯文本跑一堆正则。
func looksLikeMarkdown(s string) bool {
	return strings.ContainsAny(s, "*_`#[>"+"\n-+") || strings.Contains(s, "```")
}

// stripItalic 处理 *x* 和 _x_，但要求两侧是"文字"，
// 避免 2*3*4、snake_case_name 被误伤。
func stripItalic(s string) string {
	var out strings.Builder
	runes := []rune(s)
	n := len(runes)

	for i := 0; i < n; i++ {
		c := runes[i]
		if c != '*' && c != '_' {
			out.WriteRune(c)
			continue
		}

		// 找到下一个相同标记
		j := -1
		for k := i + 1; k < n; k++ {
			if runes[k] == c {
				j = k
				break
			}
			if runes[k] == '\n' {
				break
			}
		}
		if j <= i+1 {
			out.WriteRune(c)
			continue
		}

		inner := runes[i+1 : j]
		if !isWord(inner[0]) || !isWord(inner[len(inner)-1]) {
			out.WriteRune(c)
			continue
		}
		// 标记两侧不能是字母数字，否则可能是 2*3*4 这类算式
		if i > 0 && isDigit(runes[i-1]) {
			out.WriteRune(c)
			continue
		}
		if j+1 < n && isDigit(runes[j+1]) {
			out.WriteRune(c)
			continue
		}
		// 下划线更严格：snake_case_name 里的下划线不是强调标记
		if c == '_' {
			if i > 0 && isWord(runes[i-1]) {
				out.WriteRune(c)
				continue
			}
			if j+1 < n && isWord(runes[j+1]) {
				out.WriteRune(c)
				continue
			}
		}

		out.WriteString(string(inner))
		i = j
	}
	return out.String()
}

func isWord(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r >= 0x4E00
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
