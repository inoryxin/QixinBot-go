package webui

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xin/onebot/internal/help"
	"github.com/xin/onebot/internal/plugin"
)

// validPluginPathID 校验 URL 里的插件 id，才能拿去 filepath.Join。
// Go mux 会清理 ".."，但挡不住 %5C（反斜杠）：Windows 上
// "..%5C..%5Cx" 能逃出 runtime 目录，设置/删除接口都会被带偏。
func validPluginPathID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return false
	}
	return id == filepath.Base(id)
}

// handlePluginWeb 渲染统一的插件详情页：设置表单 + 指令说明。
// 插件不需要自带 HTML；只有需要额外交互时才提供 web/panel.html 片段。
func (s *Server) handlePluginWeb(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/plugin/")
	id, tail, _ := strings.Cut(rest, "/")
	if !validPluginPathID(id) {
		http.NotFound(w, r)
		return
	}
	// 插件自带面板：web/ 目录挂到 /plugin/<id>/panel/
	if tail == "panel" || strings.HasPrefix(tail, "panel/") {
		webDir := filepath.Join(s.Deps.RuntimeDir, id, "web")
		// panel.html 是嵌在详情页里的片段，不能当整页打开；只有 index.html 才是完整面板
		if !fileExists(filepath.Join(webDir, "index.html")) {
			http.NotFound(w, r)
			return
		}
		http.StripPrefix("/plugin/"+id+"/panel/", http.FileServer(http.Dir(webDir))).ServeHTTP(w, r)
		return
	}

	man := s.Deps.Mgr.Manifests()[id]
	if man == nil {
		http.Error(w, "插件不存在或未加载", http.StatusNotFound)
		return
	}

	values := plugin.EffectiveSettings(man, plugin.LoadSettings(s.Deps.RuntimeDir, id))
	prefixes := s.Deps.Config.Command.Prefixes

	var b strings.Builder
	fmt.Fprintf(&b, `<h1>%s</h1><p class="sub">%s · v%s · <code>%s</code></p>`,
		esc(man.Name), esc(man.Description), esc(man.Version), esc(man.ID))

	webDir := filepath.Join(s.Deps.RuntimeDir, id, "web")
	if fileExists(filepath.Join(webDir, "index.html")) && !fileExists(filepath.Join(webDir, "panel.html")) {
		fmt.Fprintf(&b, `<p><a class="primary" style="display:inline-block;padding:7px 14px;border-radius:8px;text-decoration:none" href="/plugin/%s/panel/">打开管理面板</a></p>`, esc(id))
	}

	b.WriteString(`<div class="form-box"><form method="post" action="/api/plugin/` + esc(id) + `/settings">`)
	// 告诉服务端"这次表单包含哪些 key"：没带的开关就别动，
	// 否则用脚本只提交几个字段会把其它 bool 全关掉。
	if len(man.Settings) > 0 {
		keys := make([]string, 0, len(man.Settings))
		for _, sc := range man.Settings {
			keys = append(keys, sc.Key)
		}
		fmt.Fprintf(&b, `<input type="hidden" name="_keys" value="%s">`, esc(strings.Join(keys, ",")))
	}
	b.WriteString(`<h2 style="margin-top:0">设置</h2>`)
	if len(man.Settings) == 0 {
		b.WriteString(`<p class="muted">该插件没有可配置项。</p>`)
	} else {
		for _, sc := range man.Settings {
			b.WriteString(renderSettingField(sc, values[sc.Key]))
		}
		b.WriteString(`<div style="margin-top:18px"><button class="primary" type="submit">保存</button>`)
		b.WriteString(`<span class="muted" style="margin-left:10px">保存后立即生效，不用重启</span></div>`)
	}
	b.WriteString(`</form></div>`)

	// 设置备份：主文件损坏/误保存时可以直接从轮转备份恢复。
	if backups := plugin.SettingsBackups(s.Deps.RuntimeDir, id); len(backups) > 0 {
		b.WriteString(`<h2>设置备份</h2><p class="sub">最近 4 份；恢复会先把当前设置加入备份。</p>
<table><thead><tr><th>备份</th><th>时间</th><th>大小</th><th style="width:90px">操作</th></tr></thead><tbody>`)
		for _, bak := range backups {
			name := "settings.json.bak"
			if bak.Index > 0 {
				name = fmt.Sprintf("settings.json.bak.%d", bak.Index)
			}
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>%d B</td><td><form method="post" action="/api/plugin/%s/settings/restore" style="display:inline" onsubmit="return confirm('恢复会覆盖当前设置，继续？')"><input type="hidden" name="index" value="%d"><button class="danger">恢复</button></form></td></tr>`,
				esc(name), bak.ModTime.Format("01-02 15:04:05"), bak.Size, esc(id), bak.Index)
		}
		b.WriteString(`</tbody></table>`)
	}

	// 顺序：设置 → 扩展（插件自己的交互面板）→ 指令 → 工具（折叠）
	// 工具是"查得到就行"的信息，不该占着页面底部逼人翻半天。

	// ---- 扩展：插件自带 panel.html ----
	// 面板是独立的一层：<插件>/web/panel.html（assets/web/ 是过渡写法，也认）
	for _, panel := range []string{
		filepath.Join(s.Deps.RuntimeDir, id, "web", "panel.html"),
		filepath.Join(s.Deps.RuntimeDir, id, "assets", "web", "panel.html"),
	} {
		if data, err := os.ReadFile(panel); err == nil {
			b.WriteString(`<h2>扩展</h2>`)
			b.Write(data)
			break
		}
	}

	// ---- 指令 ----
	cmds := man.Commands
	if len(cmds) == 0 {
		b.WriteString(`<h2>指令</h2><p class="muted">该插件没有注册命令。</p>`)
	} else if len(cmds) <= 5 {
		b.WriteString(`<h2>指令</h2>`)
		b.WriteString(renderCommandTable(cmds, prefixes))
	} else {
		// 命令多的默认收起，和「指令显示」页保持一致
		fmt.Fprintf(&b, `<details class="hg"><summary><span class="arrow">▶</span><strong>指令</strong> <span class="tag">%d</span></summary><div class="body">`, len(cmds))
		b.WriteString(renderCommandTable(cmds, prefixes))
		b.WriteString(`</div></details>`)
	}

	// ---- 工具（折叠）：只列**本插件**声明的工具 ----
	// 以前 use_tools=true 会把这台机器上所有插件的工具都列进来，看起来像是
	// 本插件注册了别人的工具（jm-download 这种自己带工具的插件尤其容易误解）。
	// 全量清单看 /tools（不在导航里，给 AI 类插件排查用）。
	tools := make([]plugin.ToolInfo, 0, len(man.Tools))
	for _, t := range man.Tools {
		tools = append(tools, plugin.ToolInfo{Name: t.Name, Plugin: man.ID, Description: t.Description, Parameters: t.Parameters})
	}
	if len(tools) > 0 {
		fmt.Fprintf(&b, `<details class="hg"><summary><span class="arrow">▶</span><strong>可用工具</strong> <span class="tag">%d</span></summary><div class="body">`, len(tools))
		b.WriteString(`<p class="sub" style="padding:0 10px">本插件声明给模型调用的能力。用户问相关问题时 AI 会自己调。
		全部插件的工具见 <a href="/tools">工具页</a>。</p>`)
		b.WriteString(`<table><thead><tr><th style="width:180px">工具</th><th style="width:100px">来自</th><th>说明</th><th style="width:220px">参数</th></tr></thead><tbody>`)
		for _, t := range tools {
			params := t.Parameters
			if raw, err := json.Marshal(params); err == nil {
				params = string(raw)
			} else {
				params = "{}"
			}
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td><code style="font-size:11px">%s</code></td></tr>`,
				esc(t.Name), esc(t.Plugin), esc(t.Description), esc(fmt.Sprint(params)))
		}
		b.WriteString(`</tbody></table></div></details>`)
	}

	s.render(w, man.Name, "/plugins", b.String())
}

// renderSettingField 按 schema 渲一个控件。类型未知时退化成文本框。
func renderSettingField(sc plugin.SettingSpec, value any) string {
	key := html.EscapeString(sc.Key)
	label := html.EscapeString(sc.Label)
	if label == "" {
		label = key
	}
	val := fmt.Sprintf("%v", value)
	if value == nil {
		val = ""
	}
	escVal := html.EscapeString(val)

	var b strings.Builder
	b.WriteString(`<label for="f_` + key + `">` + label + `</label>`)

	switch sc.Type {
	case "bool", "boolean": // 两种写法都认（manifest 里两种都出现过）
		// 存进来的可能是 true / "true" / "on" / 1，都要认
		on := false
		switch v := value.(type) {
		case bool:
			on = v
		case string:
			on = v == "true" || v == "on" || v == "1"
		case float64:
			on = v != 0
		}
		checked := ""
		if on {
			checked = " checked"
		}
		b.WriteString(`<div style="margin:6px 0 16px"><input type="checkbox" id="f_` + key + `" name="` + key + `"` + checked + `></div>`)
	case "select":
		b.WriteString(`<select id="f_` + key + `" name="` + key + `">`)
		for i, opt := range sc.Options {
			sel := ""
			if val == opt {
				sel = " selected"
			}
			// 有中文名就显示中文，value 仍然是原值（存的是 mention/always/prob）
			label := opt
			if i < len(sc.OptionLabels) && sc.OptionLabels[i] != "" {
				label = sc.OptionLabels[i]
			}
			b.WriteString(`<option value="` + html.EscapeString(opt) + `"` + sel + `>` + html.EscapeString(label) + `</option>`)
		}
		b.WriteString(`</select>`)
	case "number":
		extra := ""
		if sc.Min != nil {
			extra += fmt.Sprintf(` min="%v"`, *sc.Min)
		}
		if sc.Max != nil {
			extra += fmt.Sprintf(` max="%v"`, *sc.Max)
		}
		b.WriteString(`<input type="number" id="f_` + key + `" name="` + key + `" value="` + escVal + `"` + extra + `>`)
	case "text", "textarea":
		b.WriteString(`<textarea id="f_` + key + `" name="` + key + `" rows="6" style="width:100%;padding:6px 9px;border:1px solid var(--input-border);border-radius:4px;font:13px/1.5 ui-monospace,Consolas,monospace">` + html.EscapeString(val) + `</textarea>`)
	case "password":
		// 不回显真值：留空 = 不修改，填了才覆盖
		hint := "未设置，留空表示不修改"
		if val != "" {
			hint = "已设置（留空表示不修改）"
		}
		b.WriteString(`<input type="password" id="f_` + key + `" name="` + key + `" value="" placeholder="` + hint + `" autocomplete="new-password">`)
	default:
		b.WriteString(`<input type="text" id="f_` + key + `" name="` + key + `" value="` + escVal + `">`)
	}

	// 设置项说明（manifest 里的 hint）：不写的话用户不知道该填什么。
	// 用正常上边距，别用负值——负 margin 会压到输入框边框上。
	if sc.Hint != "" {
		b.WriteString(`<div class="muted" style="font-size:12px;line-height:1.5;margin:6px 0 16px">` + html.EscapeString(sc.Hint) + `</div>`)
	}
	return b.String()
}

// handlePluginSettings 读写插件设置。GET 返回 schema + 当前值，POST 保存并推给插件。
func (s *Server) handlePluginSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPluginPathID(id) {
		http.Error(w, "插件不存在", http.StatusNotFound)
		return
	}
	man := s.Deps.Mgr.Manifests()[id]
	if man == nil {
		http.Error(w, "插件不存在", http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet {
		values := plugin.EffectiveSettings(man, plugin.LoadSettings(s.Deps.RuntimeDir, id))
		writeJSON(w, http.StatusOK, map[string]any{
			"schema": man.Settings,
			"values": values,
		})
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	saved := plugin.LoadSettings(s.Deps.RuntimeDir, id)

	// 表单声明了包含哪些 key 时，只更新这些——避免"部分提交把别的开关关掉"
	present := map[string]bool{}
	if keys := r.FormValue("_keys"); keys != "" {
		for _, k := range strings.Split(keys, ",") {
			present[strings.TrimSpace(k)] = true
		}
	}

	for _, sc := range man.Settings {
		if len(present) > 0 && !present[sc.Key] {
			continue
		}
		raw := r.FormValue(sc.Key)
		switch sc.Type {
		case "number":
			if n, ok := parseFloat(raw); ok {
				saved[sc.Key] = n
			}
		case "boolean":
			saved[sc.Key] = raw == "on" || raw == "true" || raw == "1"
		case "password":
			// 密码框留空 = 没改，保留原值。
			// 不然每次点保存都把已存的密码冲成空字符串。
			if raw != "" {
				saved[sc.Key] = raw
			}
		default:
			saved[sc.Key] = raw
		}
	}

	if err := plugin.SaveSettings(s.Deps.RuntimeDir, id, saved); err != nil {
		s.Log.Error("保存插件设置失败", "plugin", id, "err", err)
		http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if h := s.Deps.Mgr.HostOf(id); h != nil {
		_ = h.Notify("plugin.set_settings", map[string]any{"values": plugin.EffectiveSettings(man, saved)})
	}

	s.Log.Info("插件设置已更新", "plugin", id, "keys", len(saved))
	// 设置里可能改了"几点推送"：由插件自行响应 set_settings 调整定时
	http.Redirect(w, r, "/plugin/"+id+"/", http.StatusSeeOther)
}

func (s *Server) handlePluginSettingsRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPluginPathID(id) {
		http.Error(w, "插件不存在", http.StatusNotFound)
		return
	}
	man := s.Deps.Mgr.Manifests()[id]
	if man == nil {
		http.Error(w, "插件不存在", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	index, err := strconv.Atoi(r.FormValue("index"))
	if err != nil {
		http.Error(w, "备份序号不合法", http.StatusBadRequest)
		return
	}
	values, err := plugin.RestoreSettingsBackup(s.Deps.RuntimeDir, id, index)
	if err != nil {
		s.Log.Error("恢复插件设置失败", "plugin", id, "index", index, "err", err)
		http.Error(w, "恢复失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if h := s.Deps.Mgr.HostOf(id); h != nil {
		_ = h.Notify("plugin.set_settings", map[string]any{"values": plugin.EffectiveSettings(man, values)})
	}
	s.Log.Info("插件设置已从备份恢复", "plugin", id, "index", index)
	http.Redirect(w, r, "/plugin/"+id+"/", http.StatusSeeOther)
}

func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	if err != nil {
		return 0, false
	}
	return f, true
}

// renderCommandTable 渲染插件详情页的指令表。
func renderCommandTable(cmds []plugin.CommandSpec, prefixes []string) string {
	var b strings.Builder
	b.WriteString(`<table><thead><tr><th style="width:320px">用法</th><th>说明</th><th style="width:120px">可见性</th></tr></thead><tbody>`)
	for _, c := range cmds {
		usage := help.FormatUsage(c.Usage, c.ID, prefixes)
		vis := `<span class="tag ok">所有人</span>`
		switch {
		case len(c.AllowUsers) > 0 || c.RequireRole != "":
			vis = `<span class="tag">管理员专属</span>`
		case c.Hidden:
			vis = `<span class="tag">不在 help 显示</span>`
		}
		fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>%s</td></tr>`,
			esc(usage), esc(c.Description), vis)
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
