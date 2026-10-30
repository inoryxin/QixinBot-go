package plugin

import (
	"strings"

	"github.com/xin/onebot/internal/session"
)

// FailureHint 在"带了前缀却没人处理"时给用户一句解释。
//
// **只解释"用户没做错什么"的情况**：
//   - 权限不够 → 找管理员（不是命令错了）
//   - 插件超时 → 稍后再试（不是用户的问题）
//   - 冷却中   → 等一会儿
//
// **命令不存在时保持沉默**：带前缀却没有对应命令，多半是打错字或
// 记混了，回一句"没有这个命令"只会显得吵（群里每打错一次就刷一条）。
// 想看有哪些命令，用户自己会发 /help。
func FailureHint(s *session.Session) string {
	if s == nil || !s.HasPrefix || s.Text == "" {
		return "" // 没带前缀的普通聊天不打扰
	}

	if s.Trace != nil {
		for _, st := range s.Trace.FailedStages() {
			if st.Name == "权限" {
				// 如果所有插件都因为"需要命令前缀"被拦，说明用户就是没带前缀
				// —— 但这里 HasPrefix 已经为 true，所以是真权限问题
				return "你没有权限使用这个命令"
			}
			if st.Name == "冷却" {
				// 明确的命令被冷却：说清楚"等多久"，别让他以为命令不存在
				if st.Reason != "" {
					return "太频繁了（" + st.Reason + "），歇一下再来"
				}
				return "太频繁了，歇一下再来"
			}
			if st.Name == "插件" && (strings.Contains(st.Reason, "timeout") ||
				strings.Contains(st.Reason, "deadline")) {
				return "处理超时了，稍后再试"
			}
		}
	}
	// 没匹配到任何命令：不响应（包括不打扰 AI）
	return ""
}
