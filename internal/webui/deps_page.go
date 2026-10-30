package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xin/onebot/internal/plugin"
)

var missingModuleRe = regexp.MustCompile(`(?:No module named|Cannot find module) ['"]([^'"]+)['"]`)

// pageDeps 是「依赖」页：官方能力 + 共享依赖 + 插件专属依赖 + 失败兜底。
func (s *Server) pageDeps(w http.ResponseWriter, r *http.Request) {
	msg := r.URL.Query().Get("msg")
	errMsg := r.URL.Query().Get("err")
	selected := r.URL.Query().Get("plugin")

	mans := s.Deps.Mgr.Manifests()
	statuses := s.Deps.Mgr.List()
	stateOf := map[string]string{}
	errOf := map[string]string{}
	pids := map[string]int{}
	for _, st := range statuses {
		stateOf[st.ID] = st.State
		if st.Error != "" {
			errOf[st.ID] = st.Error
		}
		if st.PID > 0 {
			pids[st.ID] = st.PID
		}
	}

	var b strings.Builder
	b.WriteString(`<h1>依赖管理</h1>
<p class="sub">共享依赖所有插件可用；插件专属依赖只影响一个插件。原生包按当前平台安装，插件包本身不打包平台二进制。</p>`)
	if msg != "" {
		fmt.Fprintf(&b, `<p style="color:var(--ok);background:var(--ok-bg);padding:8px 12px;border-radius:8px">%s</p>`, esc(msg))
	}
	if errMsg != "" {
		fmt.Fprintf(&b, `<p style="color:var(--bad);background:var(--bad-bg);padding:8px 12px;border-radius:8px;white-space:pre-wrap">%s</p>`, esc(errMsg))
	}

	// 框架运行时
	runtimes := s.Deps.Mgr.ListRuntimes()
	b.WriteString(`<h2>框架运行时</h2><p class="muted">上传 runtime-*.anzu（kind="runtime"）会装到 data/runtimes，不显示在插件列表；当前平台会自动设为运行时。运行自带依赖不列在下面的共享目录里。</p>`)
	b.WriteString(`<div class="form-box"><form method="post" action="/api/plugins/upload" enctype="multipart/form-data" class="row" style="align-items:flex-end;gap:8px;flex-wrap:wrap">
<label>运行时包<br><input type="file" name="file" accept=".anzu" required></label><button>上传安装</button></form></div>`)
	if len(runtimes) == 0 {
		b.WriteString(`<p class="muted">还没有安装框架运行时。</p>`)
	} else {
		b.WriteString(`<div class="form-box"><table><thead><tr><th>类型</th><th>版本</th><th>平台</th><th>状态</th><th>来源</th><th>解压后</th></tr></thead><tbody>`)
		for _, rt := range runtimes {
			status := "可用"
			if rt.Current {
				status = "✅ 当前"
			} else if rt.Platform != plugin.CurrentPlatform() {
				status = "本机不可用"
			}
			fmt.Fprintf(&b, `<tr title="%s"><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td class="muted">%s</td><td class="muted">%.0fMB</td></tr>`,
				esc(rt.Path), esc(rt.Kind), esc(rt.Version), esc(rt.Platform), esc(status), esc(rt.Source), float64(rt.SizeBytes)/1024/1024)
		}
		b.WriteString(`</tbody></table></div>`)
	}

	b.WriteString(`<h2>共享依赖</h2><p class="muted">所有插件共享，适合 Pillow、numpy、@napi-rs/canvas 这类官方/平台依赖。</p>
<div class="form-box"><div class="row" style="align-items:flex-end;gap:12px;flex-wrap:wrap">
<form method="post" action="/api/deps/install" style="display:flex;gap:6px;align-items:flex-end">
<input type="hidden" name="kind" value="python"><input type="hidden" name="scope" value="shared">
<label>Python 包<br><input name="package" placeholder="Pillow" style="width:150px"></label>
<label>版本（可空）<br><input name="version" placeholder="11.2.1" style="width:120px"></label>
<button>安装到共享</button></form>
<form method="post" action="/api/deps/install" style="display:flex;gap:6px;align-items:flex-end">
<input type="hidden" name="kind" value="node"><input type="hidden" name="scope" value="shared">
<label>Node 包<br><input name="package" placeholder="@napi-rs/canvas" style="width:180px"></label>
<label>版本（可空）<br><input name="version" placeholder="0.1.0" style="width:120px"></label>
<button>安装到共享</button></form>
</div></div>`)
	writeDepTable(&b, "Python 共享", s.Deps.Mgr, "python", "shared", "")
	writeDepTable(&b, "Node 共享", s.Deps.Mgr, "node", "shared", "")

	b.WriteString(`<h2>插件专属依赖</h2><p class="muted">某个插件需要特殊版本、或共享目录里没有时，装到该插件自己的 deps/，优先级更高。</p>
<div class="form-box"><form method="post" action="/api/deps/install" class="row" style="align-items:flex-end;gap:8px;flex-wrap:wrap">
<label>插件<br><select name="plugin" style="min-width:180px">`)
	ids := make([]string, 0, len(mans))
	for id := range mans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, esc(id), selectedAttr(id, selected), esc(id+" · "+mans[id].Name))
	}
	b.WriteString(`</select></label>
<label>类型<br><select name="kind"><option value="python">Python</option><option value="node">Node</option></select></label>
<input type="hidden" name="scope" value="plugin">
<label>包名<br><input name="package" placeholder="Pillow" style="width:150px"></label>
<label>版本（可空）<br><input name="version" placeholder="11.2.1" style="width:110px"></label>
<button>安装到插件</button></form></div>`)
	if selected != "" && mans[selected] != nil {
		writeDepTable(&b, "插件 "+selected+" · Python", s.Deps.Mgr, "python", "plugin", selected)
		writeDepTable(&b, "插件 "+selected+" · Node", s.Deps.Mgr, "node", "plugin", selected)
	}

	var failed []string
	for id, st := range stateOf {
		if st == "failed" {
			failed = append(failed, id)
		}
	}
	sort.Strings(failed)
	b.WriteString(`<h2>安装失败 / 缺依赖</h2><p class="muted">没声明 download、启动时报缺模块的插件会出现在这里；手动装到插件目录即可，不用重新打包。</p>`)
	if len(failed) == 0 {
		b.WriteString(`<p class="muted">当前没有失败的插件。</p>`)
	} else {
		b.WriteString(`<div class="form-box"><table><thead><tr><th>插件</th><th>错误</th><th>手动补依赖</th></tr></thead><tbody>`)
		for _, id := range failed {
			errText := errOf[id]
			pkg := ""
			if m := missingModuleRe.FindStringSubmatch(errText); len(m) > 1 {
				pkg = m[1]
				if i := strings.IndexAny(pkg, "./"); i > 0 {
					pkg = pkg[:i]
				}
			}
			fmt.Fprintf(&b, `<tr><td><strong>%s</strong><br><code>%s</code></td><td class="muted" style="max-width:420px;white-space:pre-wrap">%s</td><td>
<form method="post" action="/api/deps/install" style="display:flex;gap:4px;align-items:center">
<input type="hidden" name="scope" value="plugin"><input type="hidden" name="plugin" value="%s">
<select name="kind"><option value="python">Python</option><option value="node">Node</option></select>
<input name="package" value="%s" placeholder="包名" style="width:130px">
<input name="version" placeholder="版本" style="width:80px">
<button>装到该插件</button></form></td></tr>`,
				esc(id), esc(id), esc(errText), esc(id), esc(pkg))
		}
		b.WriteString(`</tbody></table></div>`)
	}

	// 功能插件依赖检查：比如 ai-voice 需要 ai-chat，缺了就在这里提示
	b.WriteString(`<h2>功能插件依赖</h2><p class="muted">功能插件之间的依赖检查；缺了先装/启用被依赖的插件。</p>`)
	var depRows []string
	for _, id := range ids {
		man := mans[id]
		if man.Kind == "service" {
			continue
		}
		for _, req := range man.Requires {
			if req.Kind != "plugin" || req.Value == "" {
				continue
			}
			state := stateOf[req.Value]
			if state == "ready" {
				continue
			}
			if state == "" {
				state = "未安装"
			}
			depRows = append(depRows, fmt.Sprintf(`<tr><td><strong>%s</strong><br><code>%s</code></td><td><code>%s</code></td><td>%s</td><td class="muted">%s</td></tr>`,
				esc(man.Name), esc(id), esc(req.Value), esc(state), esc(req.Name)))
		}
	}
	if len(depRows) == 0 {
		b.WriteString(`<p class="muted">所有功能插件的依赖都满足。</p>`)
	} else {
		b.WriteString(`<div class="form-box"><table><thead><tr><th>插件</th><th>需要</th><th>状态</th><th>说明</th></tr></thead><tbody>`)
		b.WriteString(strings.Join(depRows, ""))
		b.WriteString(`</tbody></table></div>`)
	}

	b.WriteString(`<h2>官方能力</h2><p class="muted">只列底层服务（数据库、画图、LLM、生活状态）。功能插件（ai-chat/ai-voice 等）不在这里，它们之间缺依赖会在上面提示。</p>`)
	b.WriteString(`<div class="form-box"><table><thead><tr><th>依赖</th><th>状态</th><th>PID</th><th>提供的能力</th><th>说明</th></tr></thead><tbody>`)
	capIDs := make([]string, 0)
	for id, man := range mans {
		// 只显示底层服务插件；功能插件不当作"官方依赖"
		if man.Kind == "service" {
			capIDs = append(capIDs, id)
		}
	}
	sort.Strings(capIDs)
	for _, id := range capIDs {
		man := mans[id]
		state := stateOf[id]
		if state == "" {
			state = "disabled"
		}
		pid := ""
		if pids[id] > 0 {
			pid = fmt.Sprintf("%d", pids[id])
		}
		fmt.Fprintf(&b, `<tr><td><a href="/plugin/%s/"><strong>%s</strong></a><br><code>%s</code></td><td>%s</td><td class="muted">%s</td><td class="muted">%s</td><td class="muted">%s</td></tr>`,
			esc(id), esc(man.Name), esc(id), esc(state), pid,
			esc(strings.Join(man.Provides, "、")), esc(man.Description))
	}
	b.WriteString(`<tr><td><strong>内置 SQLite</strong><br><code>db.sqlite</code></td><td>builtin</td><td class="muted">—</td><td class="muted">db.sqlite</td><td class="muted">框架内置，不需要装插件</td></tr>`)
	b.WriteString(`</tbody></table></div>`)

	s.render(w, "依赖", "/deps", b.String())
}

func selectedAttr(id, selected string) string {
	if id == selected {
		return " selected"
	}
	return ""
}

func writeDepTable(b *strings.Builder, title string, mgr *plugin.Manager, kind, scope, pluginID string) {
	items, err := mgr.ListDependencies(kind, scope, pluginID)
	fmt.Fprintf(b, `<h3>%s</h3>`, esc(title))
	if err != nil {
		fmt.Fprintf(b, `<p class="muted">读取失败：%s</p>`, esc(err.Error()))
		return
	}
	if len(items) == 0 {
		b.WriteString(`<p class="muted">（空）</p>`)
		return
	}
	b.WriteString(`<div class="form-box"><table><thead><tr><th>包</th><th>版本</th></tr></thead><tbody>`)
	for _, it := range items {
		fmt.Fprintf(b, `<tr><td><code>%s</code></td><td class="muted">%s</td></tr>`, esc(it.Name), esc(it.Version))
	}
	b.WriteString(`</tbody></table></div>`)
}

// apiInstallDep 处理依赖安装；安装可能需要几分钟，管理员在页面上等待结果。
func (s *Server) apiInstallDep(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	scope := strings.TrimSpace(r.FormValue("scope"))
	pluginID := strings.TrimSpace(r.FormValue("plugin"))
	pkg := strings.TrimSpace(r.FormValue("package"))
	version := strings.TrimSpace(r.FormValue("version"))
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	if _, err := s.Deps.Mgr.InstallDependency(ctx, kind, scope, pluginID, pkg, version); err != nil {
		http.Redirect(w, r, "/deps?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	target := "共享依赖"
	if scope == "plugin" {
		target = "插件 " + pluginID
	}
	http.Redirect(w, r, "/deps?msg="+url.QueryEscape("已安装 "+pkg+" 到 "+target)+"&plugin="+url.QueryEscape(pluginID), http.StatusSeeOther)
}
