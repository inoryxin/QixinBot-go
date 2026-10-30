package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// apiTools 返回所有插件注册的工具。
func (s *Server) apiTools(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"tools": s.Deps.Mgr.ListTools()})
}

// apiCapabilities 列出所有能力及其提供者（按优先级排序）。
func (s *Server) apiCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"capabilities": s.Deps.Mgr.ListCapabilities(),
		// 框架内置能力（不走插件），单独给一份，前端可以标成「框架」
		"builtins": s.Deps.Mgr.BuiltinNames(),
	})
}

// apiCallCapability 直接调一个能力，用于验证"覆盖关系"和排查。
func (s *Server) apiCallCapability(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest)
		return
	}
	args := map[string]any{}
	if raw := r.FormValue("args"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			http.Error(w, "args 不是合法 JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	start := time.Now()
	data, err := s.Deps.Mgr.CallCapability(ctx, name, args, nil)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "ms": elapsed})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": json.RawMessage(data), "ms": elapsed})
}

// apiCallTool 手动调用一个工具，用于验证插件是否正确实现。
func (s *Server) apiCallTool(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "缺少工具名", http.StatusBadRequest)
		return
	}

	args := map[string]any{}
	if raw := strings.TrimSpace(r.FormValue("args")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "参数不是合法 JSON: " + err.Error()})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	start := time.Now()
	data, err := s.Deps.Mgr.CallTool(ctx, name, args, nil)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "ms": elapsed})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": json.RawMessage(data), "ms": elapsed})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) pageTools(w http.ResponseWriter, r *http.Request) {
	tools := s.Deps.Mgr.ListTools()

	var b strings.Builder
	b.WriteString(`<h1>工具</h1><p class="sub">插件声明给 AI 调用的能力；也可以在这里手动调用验证</p>`)

	if len(tools) == 0 {
		b.WriteString(`<div class="notice">当前没有任何插件声明工具。装上 AI 类插件后这里才会有内容。</div>`)
	} else {
		b.WriteString(`<table><thead><tr><th>工具名</th><th>插件</th><th>说明</th><th style="width:90px"></th></tr></thead><tbody>`)
		for _, t := range tools {
			params, _ := json.MarshalIndent(t.Parameters, "", "  ")
			fmt.Fprintf(&b, `<tr>
<td><code>%s</code><div class="muted" style="max-width:420px;white-space:pre-wrap">%s</div></td>
<td>%s</td>
<td class="muted">%s</td>
<td><button onclick="pick('%s')">测试</button></td>
</tr>`, esc(t.Name), esc(string(params)), esc(t.Plugin), esc(t.Description), esc(t.Name))
		}
		b.WriteString(`</tbody></table>`)
	}

	// ---- 后台任务 ----
	jobs := s.Deps.Mgr.ListJobs()
	b.WriteString(`<h2>后台任务</h2><p class="sub">插件用 host.background 起的任务，不阻塞消息处理</p>`)
	if len(jobs) == 0 {
		b.WriteString(`<div class="notice">没有正在跑的后台任务。</div>`)
	} else {
		b.WriteString(`<table><thead><tr><th>插件</th><th>任务</th><th>会话</th><th>开始</th><th>状态</th></tr></thead><tbody>`)
		for _, j := range jobs {
			state := `<span class="ok">运行中</span>`
			if j.Done {
				if j.Err != "" {
					state = `<span class="bad">失败</span> <span class="muted" style="font-size:11px">` + esc(j.Err) + `</span>`
				} else {
					state = `<span class="muted">已完成</span>`
				}
			}
			chat := "—"
			if j.ChatType != "" {
				chat = fmt.Sprintf("%s:%d", j.ChatType, j.ChatID)
			}
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td class="muted">%s</td><td class="muted">%s</td><td>%s</td></tr>`,
				esc(j.Plugin), esc(j.Name), esc(chat),
				time.Unix(j.StartedAt, 0).Format("15:04:05"), state)
		}
		b.WriteString(`</tbody></table>`)
	}

	// 手动调用面板：直接调 /api/tools/call，不经过页面刷新，方便连续测
	b.WriteString(`<h2>手动调用</h2>
<div class="form-box">
  <label>工具名</label><input id="tname" type="text" placeholder="weather_query">
  <label>参数（JSON）</label><textarea id="targs" rows="5" style="width:100%;padding:8px;border:1px solid var(--input-border);border-radius:4px;font:12px ui-monospace,monospace">{}</textarea>
  <div style="margin-top:14px"><button class="primary" onclick="callTool()">调用</button><span id="tmsg" style="margin-left:10px"></span></div>
</div>
<pre id="tresult" class="logbox" style="margin-top:14px;height:auto;min-height:60px;display:none"></pre>

<script>
function pick(name){ document.getElementById('tname').value = name; document.getElementById('targs').focus(); }
async function callTool(){
  const name = document.getElementById('tname').value.trim();
  const args = document.getElementById('targs').value || '{}';
  const msg = document.getElementById('tmsg');
  const out = document.getElementById('tresult');
  if(!name){ msg.textContent = '先填工具名'; return; }
  msg.textContent = '调用中…';
  try {
    const body = new URLSearchParams({ name, args });
    const r = await fetch('/api/tools/call', { method:'POST', body });
    const data = await r.json();
    msg.textContent = data.ok ? ('成功 · ' + data.ms + 'ms') : ('失败 · ' + data.ms + 'ms');
    out.style.display = 'block';
    out.textContent = JSON.stringify(data, null, 2);
  } catch(e) {
    msg.textContent = '请求失败: ' + e.message;
  }
}
</script>`)

	s.render(w, "工具", "/tools", b.String())
}
