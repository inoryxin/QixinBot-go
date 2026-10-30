package webui

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// pageAudit 展示最近的审计记录。
func (s *Server) pageAudit(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(`<h1>审计</h1><p class="sub">谁在什么时候、对哪个会话、做了什么。按天写 JSONL，保留最近 7 天。</p>`)

	entries := s.Deps.Audit.Recent(300)
	if len(entries) == 0 {
		b.WriteString(`<div class="notice">还没有记录。发一条命令试试。</div>`)
		s.render(w, "审计", "/audit", b.String())
		return
	}

	allowed, denied := 0, 0
	for _, e := range entries {
		if e.Allowed {
			allowed++
		} else {
			denied++
		}
	}

	fmt.Fprintf(&b, `<div class="cards">
<div class="card"><div class="k">最近记录</div><div class="v">%d</div></div>
<div class="card"><div class="k">通过</div><div class="v">%d</div></div>
<div class="card"><div class="k">拒绝</div><div class="v">%d</div></div>
</div>`, len(entries), allowed, denied)

	b.WriteString(`<table><thead><tr>
<th style="width:70px">时间</th><th style="width:66px">动作</th><th style="width:80px">插件</th>
<th style="width:170px">会话</th><th>详情</th><th style="width:60px">结果</th>
</tr></thead><tbody>`)

	for _, e := range entries {
		ts := time.Unix(e.TS, 0).Format("15:04:05")
		scope := "—"
		if e.ChatID != 0 {
			scope = fmt.Sprintf("%s %d", e.ChatType, e.ChatID)
			if e.UserID != 0 {
				scope += fmt.Sprintf("<br><span class=\"muted\">%d</span>", e.UserID)
			}
		}
		result := `<span class="tag ok">通过</span>`
		if !e.Allowed {
			result = `<span class="tag bad">拒绝</span>`
		}
		// Detail 是原始消息文本（任意群友可控），必须转义——
		// 不转义就是存储型 XSS：一条消息拿走整个控制台
		detail := esc(e.Detail)
		if e.Reason != "" {
			detail += `<br><span class="muted">` + esc(e.Reason) + `</span>`
		}
		if detail == "" {
			detail = `<span class="muted">—</span>`
		}

		fmt.Fprintf(&b, `<tr><td class="muted">%s</td><td>%s</td><td>%s</td><td class="muted">%s</td><td>%s</td><td>%s</td></tr>`,
			esc(ts), esc(e.Action), esc(e.Plugin), scope, detail, result)
	}
	b.WriteString(`</tbody></table>`)

	s.render(w, "审计", "/audit", b.String())
}

var _ = strings.TrimSpace
