package webui

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xin/onebot/internal/plugin"
	"github.com/xin/onebot/internal/registry"
)

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request, path string) {
	switch path {
	case "/overview":
		s.pageOverview(w, r)
	case "/connect":
		s.pageConnect(w, r)
	case "/permissions":
		s.pagePermissions(w, r)
	case "/plugins":
		s.pagePlugins(w, r)
	case "/cohost":
		s.pageCohost(w, r)
	case "/help-settings":
		s.pageHelpSettings(w, r)
	case "/logs":
		s.pageLogs(w, r)
	case "/tools":
		s.pageTools(w, r)
	case "/chat":
		s.pageChat(w, r)
	case "/audit":
		s.pageAudit(w, r)
	default:
		http.NotFound(w, r)
	}
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

// stateTag 状态标签：ready 绿、disabled 中性、其余（failed/缺依赖等）红；
// 共享宿主插件加"共享"标记。
func stateTag(st plugin.Status) string {
	cls := "bad"
	switch st.State {
	case "ready":
		cls = "ok"
	case "disabled":
		cls = ""
	}
	shared := ""
	if st.Shared {
		shared = ` <span class="tag">共享</span>`
	}
	return `<span class="tag ` + cls + `">` + esc(st.State) + `</span>` + shared
}

// memCell 内存单元格：0 = 进程不在，显示 —。
func memCell(mb float64) string {
	if mb <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f MB", mb)
}

func (s *Server) pageOverview(w http.ResponseWriter, r *http.Request) {
	d := s.Deps
	var b strings.Builder

	fmt.Fprintf(&b, `<h1>概览</h1><p class="sub">运行时长 %s · 版本 %s</p>`, humanDuration(time.Since(d.StartedAt)), esc(d.Version))

	// ---- 服务器资源 ----
	if d.Monitor != nil {
		if snap, ok := d.Monitor.Last(); ok {
			b.WriteString(`<style>
.res{display:flex;gap:12px;flex-wrap:wrap;margin-bottom:20px}
.res-card{flex:1;min-width:180px;border:1px solid var(--border);border-radius:10px;background:var(--panel);padding:12px 14px}
.res-name{font-size:12px;color:var(--muted);margin-bottom:6px}
.res-val{font-size:22px;font-weight:600;line-height:1.2}
.res-sub{font-size:11px;color:var(--muted);margin-top:4px}
.res-bar{height:5px;border-radius:3px;background:var(--hover);margin-top:8px;overflow:hidden}
.res-bar i{display:block;height:100%;border-radius:3px}
</style>
<div class="res">`)

			bar := func(p float64) string {
				colour := "var(--accent)"
				if p >= 90 {
					colour = "var(--bad)"
				} else if p >= 70 {
					colour = "var(--warn)"
				}
				if p > 100 {
					p = 100
				}
				return fmt.Sprintf(`<div class="res-bar"><i style="width:%.1f%%;background:%s"></i></div>`, p, colour)
			}
			gb := func(n uint64) string {
				return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
			}

			// 内存用自适应单位：小于 1 GB 显示 MB，避免 25 MB 被显示成 0.0 GB
			mem := func(n uint64) string {
				mb := float64(n) / (1 << 20)
				if mb < 1024 {
					return fmt.Sprintf("%.0f MB", mb)
				}
				return fmt.Sprintf("%.1f GB", mb/1024)
			}

			fmt.Fprintf(&b, `<div class="res-card">
<div class="res-name">CPU</div>
<div class="res-val">%.1f%%</div>
%s
<div class="res-sub">%d 核 · %s/%s</div>
</div>`, snap.CPUPercent, bar(snap.CPUPercent),
				snap.CPUCores, esc(snap.OS), esc(snap.Arch))

			fmt.Fprintf(&b, `<div class="res-card">
<div class="res-name">内存</div>
<div class="res-val">%.1f%%</div>
%s
<div class="res-sub">已用 %s / 共 %s</div>
</div>`, snap.MemPercent, bar(snap.MemPercent), gb(snap.MemUsed), gb(snap.MemTotal))

			fmt.Fprintf(&b, `<div class="res-card">
<div class="res-name">磁盘</div>
<div class="res-val">%.1f%%</div>
%s
<div class="res-sub">已用 %s / 共 %s</div>
</div>`, snap.DiskPercent, bar(snap.DiskPercent), gb(snap.DiskUsed), gb(snap.DiskTotal))

			// 插件是独立子进程，必须和框架分开算、合起来看
			totalRSS := snap.SelfRSS + snap.PluginRSS
			fmt.Fprintf(&b, `<div class="res-card">
<div class="res-name">框架 + 插件</div>
<div class="res-val">%s</div>
<div class="res-sub">框架 %s · 插件 %d 进程 %s · %d goroutine</div>
</div>`, mem(totalRSS), mem(snap.SelfRSS), snap.PluginProcs, mem(snap.PluginRSS), snap.GoRoutines)

			b.WriteString(`</div>`)
		}
	}

	plugins := d.Mgr.List()
	ready, failed, disabled := 0, 0, 0
	for _, p := range plugins {
		switch p.State {
		case "ready":
			ready++
		case "disabled":
			disabled++
		default:
			failed++
		}
	}
	features := d.Reg.All()

	fmt.Fprintf(&b, `<div class="cards">
<div class="card"><div class="k">插件</div><div class="v">%d</div><div class="muted">%d 运行 / %d 禁用 / %d 异常</div></div>
<div class="card"><div class="k">功能</div><div class="v">%d</div><div class="muted">命令与工具</div></div>
<div class="card"><div class="k">账号</div><div class="v">%d</div><div class="muted">OneBot 连接</div></div>
</div>`, len(plugins), ready, disabled, failed, len(features), len(d.Config.Accounts))

	snowHost := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		snowHost = h
	}
	fmt.Fprintf(&b, `<div style="margin:4px 0 8px"><a href="http://%s:5099" target="_blank"><button>打开 SnowLuma</button></a></div>`, esc(snowHost))
	b.WriteString(`<h2>连接状态</h2><table><thead><tr><th>账号</th><th>地址</th><th>状态</th><th>QQ</th><th>昵称</th></tr></thead><tbody>`)
	for _, a := range d.Config.Accounts {
		client := d.Clients[a.ID]
		state := `<span class="tag bad">未连接</span>`
		selfID, nick := "", ""
		if client != nil {
			if client.Connected() {
				state = `<span class="tag ok">已连接</span>`
			}
			if id := client.SelfID(); id != 0 {
				selfID = fmt.Sprintf("%d", id)
			}
			nick = client.Nickname()
		}
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			esc(a.ID), esc(a.WSURL), state, esc(selfID), esc(nick))
	}
	b.WriteString(`</tbody></table>`)

	// 最近错误
	errs := 0
	var rows strings.Builder
	if d.Logs != nil {
		for _, e := range d.Logs.Since(0, 500) {
			if e.Level != "ERROR" {
				continue
			}
			errs++
			fmt.Fprintf(&rows, `<tr><td class="muted">%s</td><td class="err">%s</td><td class="muted">%s</td></tr>`,
				esc(e.Time), esc(e.Msg), esc(e.Attrs))
		}
	}
	// 运行时环境：装没装、版本多少
	b.WriteString(`<h2>运行时环境</h2><div class="cards">`)
	for _, rt := range plugin.DetectAllRuntimes() {
		cls := "ok"
		hint := ""
		if !rt.OK {
			cls = "bad"
			hint = fmt.Sprintf(`<div class="muted" style="font-size:12px;margin-top:6px">%s</div>`, esc(rt.Message))
		}
		value := rt.Version
		if value == "" {
			value = "未装"
		}
		// 只显示来源；解释器路径由运行时包/自动探测决定，不再让用户手填
		fmt.Fprintf(&b, `<div class="card"><div class="k">%s</div><div class="v %s">%s</div>%s`, esc(rt.Name), cls, esc(value), hint)
		if rt.Kind != "" {
			fmt.Fprintf(&b, `<div class="muted" style="margin-top:8px;font-size:12px">来源：%s</div>`, esc(rt.Source))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)

	// 服务插件单独一块：它们只提供能力，不接消息
	var services, normal []plugin.Status
	for _, p := range plugins {
		if s.Deps.Mgr.IsService(p.ID) {
			services = append(services, p)
		} else {
			normal = append(normal, p)
		}
	}

	if len(services) > 0 {
		b.WriteString(`<h2>服务插件</h2><p class="sub">只提供能力（数据库、画图、AI…），不接消息</p>`)
		b.WriteString(`<table><thead><tr><th>插件</th><th>状态</th><th>PID</th><th>提供的能力</th><th>内存</th></tr></thead><tbody>`)
		for _, p := range services {
			pid := ""
			if p.PID > 0 {
				pid = fmt.Sprintf("%d", p.PID)
			}
			caps := strings.Join(s.Deps.Mgr.CapabilitiesOf(p.ID), "、")
			if caps == "" {
				caps = "—"
			}
			fmt.Fprintf(&b, `<tr><td>%s <span class="muted">%s</span></td><td>%s</td><td class="muted">%s</td><td class="muted">%s</td><td class="muted">%s</td></tr>`,
				esc(p.ID), esc(p.Name), stateTag(p), pid, esc(caps), memCell(p.MemoryMB))
		}
		b.WriteString(`</tbody></table>`)
	}

	statsAll := s.Deps.Mgr.AllStats()
	b.WriteString(`<h2>运行中的插件</h2><p class="sub">调用次数、平均耗时、崩溃次数反映插件的实际健康度</p>`)
	b.WriteString(`<table><thead><tr><th>插件</th><th>状态</th><th>PID</th><th>命令数</th><th>调用</th><th>平均耗时</th><th>崩溃</th><th>内存</th></tr></thead><tbody>`)
	for _, p := range normal {
		pid := ""
		if p.PID > 0 {
			pid = fmt.Sprintf("%d", p.PID)
		}
		st := statsAll[p.ID]
		avg := "—"
		if st.Calls > 0 {
			avg = fmt.Sprintf("%dms", st.AvgMS())
		}
		calls := "—"
		if st.Calls > 0 {
			calls = fmt.Sprintf("%d", st.Calls)
		}
		crashes := "—"
		if st.Crashes > 0 {
			crashes = fmt.Sprintf(`<span class="bad">%d</span>`, st.Crashes)
		}
		fmt.Fprintf(&b, `<tr><td>%s <span class="muted">%s</span></td><td>%s</td><td class="muted">%s</td><td>%d</td><td>%s</td><td class="muted">%s</td><td>%s</td><td class="muted">%s</td></tr>`,
			esc(p.ID), esc(p.Name), stateTag(p), pid, p.Commands, calls, avg, crashes, memCell(p.MemoryMB))
	}
	b.WriteString(`</tbody></table>`)

	// 24 小时趋势（/api/metrics 的 5 分钟窗口，纯 SVG 不引库）
	b.WriteString(`<h2>24 小时趋势</h2>
<div class="cards">
<div class="card" style="grid-column:1/-1"><div class="k">消息与回复</div><svg id="trend-msg" width="100%" height="120" preserveAspectRatio="none" viewBox="0 0 720 120"></svg></div>
<div class="card" style="grid-column:1/-1"><div class="k">插件平均耗时 (ms)</div><svg id="trend-lat" width="100%" height="100" preserveAspectRatio="none" viewBox="0 0 720 100"></svg></div>
<div class="card" style="grid-column:1/-1"><div class="k">框架内存 (MB)</div><svg id="trend-mem" width="100%" height="80" preserveAspectRatio="none" viewBox="0 0 720 80"></svg></div>
</div>
<script>
(async function(){
  try {
    const r = await fetch('/api/metrics');
    const d = await r.json();
    const W = d.windows || [];
    if (!W.length) return;
    const line = (svg, vals, color) => {
      if (!vals.length) return;
      const el = document.getElementById(svg); if (!el) return;
      const max = Math.max(1, ...vals);
      const pts = vals.map((v,i) => (i/(vals.length-1||1))*720 + ',' + (100 - (v/max)*92));
      // 只有一个窗口时 polyline 只有一个点，浏览器不会画线；补圆点让首窗也可见。
      const dots = pts.map((p) => {
        const xy = p.split(',');
        return '<circle cx="' + xy[0] + '" cy="' + xy[1] + '" r="2.5" fill="' + color + '"/>';
      }).join('');
      el.innerHTML = '<polyline fill="none" stroke="' + color + '" stroke-width="1.5" points="' + pts.join(' ') + '"/>' +
        dots +
        '<text x="6" y="12" fill="var(--muted,#888)" font-size="11">峰 ' + max + '</text>';
    };
    line('trend-msg', W.map(w => w.msg_in), 'var(--ok,#2a9d5c)');
    line('trend-lat', W.filter(w => w.avg_ms > 0).map(w => w.avg_ms), 'var(--accent,#4a7dbd)');
    line('trend-mem', W.filter(w => w.mem_mb > 0).map(w => w.mem_mb), 'var(--warn,#c9803a)');
  } catch(e) { /* 指标是锦上添花，取不到就不画 */ }
})();
</script>`)

	if errs > 0 {
		fmt.Fprintf(&b, `<h2>最近错误（%d）</h2><table><thead><tr><th>时间</th><th>消息</th><th>字段</th></tr></thead><tbody>%s</tbody></table>`, errs, rows.String())
	}

	// 界面主题放页面最底：首页首屏留给连接/插件/运行时这些关键信息
	packs := s.listThemePacks()
	current := d.Config.Web.ThemePack
	opts := `<option value="">内置默认</option>`
	for _, p := range packs {
		sel := ""
		if p.ID == current {
			sel = " selected"
		}
		label := p.Name
		if p.Builtin {
			label += "（内置）"
		}
		opts += `<option value="` + esc(p.ID) + `"` + sel + `>` + esc(label) + `</option>`
	}
	themeMsg := ""
	switch r.FormValue("theme") {
	case "ok":
		themeMsg = "主题已切换，刷新页面生效。"
	case "uploaded":
		themeMsg = "主题包已安装，在上方选择即可使用。"
	case "removed":
		themeMsg = "主题包已删除。"
	case "fail":
		themeMsg = "主题包安装失败：" + r.FormValue("msg")
	}
	if themeMsg != "" {
		b.WriteString(`<div class="notice">` + esc(themeMsg) + `</div>`)
	}

	b.WriteString(`<h2>界面主题</h2>
<div class="form-box"><form method="post" action="/api/theme/select">
<label>主题包</label>
<select name="pack">` + opts + `</select>
<div style="margin-top:14px"><button class="primary" type="submit">使用该主题</button></div>
</form>
<div style="margin-top:14px"><form method="post" action="/api/theme/upload" enctype="multipart/form-data">
<label>上传主题包（kind="theme" 的 .anzu，8MB 以内，需签名或开发模式）</label>
<input type="file" name="file" accept=".anzu">
<div style="margin-top:10px"><button type="submit">上传安装</button></div>
</form></div>
<table style="margin-top:14px"><thead><tr><th>已安装主题包</th><th>版本</th><th style="width:70px"></th></tr></thead><tbody>`)
	hasPack := false
	for _, p := range packs {
		if p.Builtin {
			continue
		}
		hasPack = true
		b.WriteString(`<tr><td>` + esc(p.Name) + ` <span class="muted">` + esc(p.ID) + `</span></td><td class="muted">` + esc(p.Version) + `</td><td><form method="post" action="/api/theme/remove" class="js-delete"><input type="hidden" name="id" value="` + esc(p.ID) + `"><button type="submit">删除</button></form></td></tr>`)
	}
	if !hasPack {
		b.WriteString(`<tr><td colspan="3" class="muted">还没有安装主题包。做一个 kind="theme" 的 .anzu（manifest.toml + theme.css + assets/）传上来就行。</td></tr>`)
	}
	b.WriteString(`</tbody></table></div>`)

	s.render(w, "概览", "/overview", b.String())
}

func (s *Server) pageConnect(w http.ResponseWriter, r *http.Request) {
	d := s.Deps
	if len(d.Config.Accounts) == 0 {
		// apiSaveConnect 对同样情况是显式报错的，这里不能 panic：
		// 空账号配置时整页 500 比报错难排查得多
		s.render(w, "连接", "/connect", `<h1>连接设置</h1><div class="notice">配置里还没有任何 [[accounts]]，请先编辑 config.toml 添加一个账号再重启。</div>`)
		return
	}
	a := d.Config.Accounts[0]

	var b strings.Builder
	b.WriteString(`<h1>连接设置</h1><p class="sub">修改后需要重启框架生效（进程会重连）</p>
<div class="form-box"><form method="post" action="/api/connect">
<label>OneBot WebSocket 地址</label>
<input type="text" name="ws_url" value="` + esc(a.WSURL) + `" placeholder="ws://127.0.0.1:3001">
<label>访问令牌（token）</label>
<input type="text" name="access_token" value="` + esc(a.AccessToken) + `" placeholder="留空表示不鉴权">
<label>HTTP API 地址（可选）</label>
<input type="text" name="http_url" value="` + esc(a.HTTPURL) + `" placeholder="http://127.0.0.1:3000">
<label>HTTP 令牌（可选）</label>
<input type="text" name="http_token" value="` + esc(a.HTTPToken) + `">
<div style="margin-top:18px"><button class="primary" type="submit">保存</button></div>
</form></div>`)

	snowHost := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		snowHost = h
	}
	fmt.Fprintf(&b, `<div style="margin:4px 0 8px"><a href="http://%s:5099" target="_blank"><button>打开 SnowLuma</button></a></div>`, esc(snowHost))
	b.WriteString(`<h2>当前连接</h2><table><thead><tr><th>项</th><th>值</th></tr></thead><tbody>`)
	client := d.Clients[a.ID]
	state := "未连接"
	if client != nil && client.Connected() {
		state = "已连接"
	}
	fmt.Fprintf(&b, `<tr><td>状态</td><td>%s</td></tr>`, esc(state))
	fmt.Fprintf(&b, `<tr><td>QQ 号</td><td>%d</td></tr>`, client.SelfID())
	fmt.Fprintf(&b, `<tr><td>昵称</td><td>%s</td></tr>`, esc(client.Nickname()))
	// 最近活动：连接"活着"但对端挂起时（心跳正常、业务层死了），
	// 这里是唯一的可见信号——最后收帧/最后事件时间一目了然
	lastRecv := time.Unix(0, client.LastRecvAt())
	lastEvent := "从未收到"
	if t := client.LastEventAt(); t != 0 {
		lastEvent = time.Unix(0, t).Format("15:04:05")
	}
	stale := ""
	if client.Connected() && client.LastEventAt() != 0 &&
		time.Since(time.Unix(0, client.LastEventAt())) > 30*time.Minute {
		stale = ` <span class="tag bad">对端可能已挂起</span>`
	}
	fmt.Fprintf(&b, `<tr><td>最近收帧</td><td>%s</td></tr>`, esc(lastRecv.Format("15:04:05")))
	fmt.Fprintf(&b, `<tr><td>最近事件</td><td>%s%s</td></tr>`, esc(lastEvent), stale)
	b.WriteString(`</tbody></table>`)

	if msg := r.URL.Query().Get("saved"); msg == "1" {
		b.WriteString(`<div class="notice">已保存到 config.toml，重启框架后生效。</div>`)
	}

	b.WriteString(`<h2>登录密码</h2>
<div class="form-box"><form method="post" action="/api/password">
<label>当前密码</label><input type="password" name="old" autocomplete="current-password">
<label>新密码（至少 6 位）</label><input type="password" name="new" autocomplete="new-password">
<label>再输一次</label><input type="password" name="confirm" autocomplete="new-password">
<div style="margin-top:18px"><button class="primary" type="submit">改密码</button></div>
</form></div>`)

	switch r.URL.Query().Get("pwd") {
	case "ok":
		b.WriteString(`<div class="notice" style="background:var(--ok-bg);border-color:var(--ok);color:var(--ok)">密码已更新，下次登录用新密码。</div>`)
	case "badold":
		b.WriteString(`<div class="notice" style="background:var(--bad-bg);border-color:var(--bad-border);color:var(--bad)">当前密码不对。</div>`)
	case "short":
		b.WriteString(`<div class="notice" style="background:var(--bad-bg);border-color:var(--bad-border);color:var(--bad)">新密码至少 6 位。</div>`)
	case "mismatch":
		b.WriteString(`<div class="notice" style="background:var(--bad-bg);border-color:var(--bad-border);color:var(--bad)">两次输入不一致。</div>`)
	case "savefail":
		b.WriteString(`<div class="notice" style="background:var(--bad-bg);border-color:var(--bad-border);color:var(--bad)">写入 config.toml 失败，看日志。</div>`)
	}

	s.render(w, "连接", "/connect", b.String())
}

func (s *Server) pagePermissions(w http.ResponseWriter, r *http.Request) {
	d := s.Deps
	var b strings.Builder
	// 主人 QQ：给"仅主人"快捷按钮用
	var ownerQQ int64
	if len(d.Config.Owner.Users) > 0 {
		ownerQQ = d.Config.Owner.Users[0]
	}

	b.WriteString(`<h1>命令权限</h1><p class="sub">一个插件一套控制。前缀和 @ 是「必需条件」：关掉后加了也认。多个 QQ / 群用 <code>|</code> 分割。所有者始终放行。</p>`)

	// 主人 QQ：框架级最高权限，改这里所有插件都放行
	var ownerVal string
	{
		parts := make([]string, 0, len(d.Config.Owner.Users))
		for _, u := range d.Config.Owner.Users {
			parts = append(parts, strconv.FormatInt(u, 10))
		}
		ownerVal = strings.Join(parts, "|")
	}
	b.WriteString(`<div class="form-box" style="margin-bottom:20px"><form method="post" action="/api/owner">`)
	b.WriteString(`<h2 style="margin-top:0">主人 QQ</h2>`)
	b.WriteString(`<p class="muted" style="margin:0 0 10px">框架级最高权限：填在里面的 QQ，<strong>所有插件都对他放行</strong>，不受下面的允许列表限制。聊天页「虚拟群」的主人身份也用它。多个用 <code>|</code> 分隔。</p>`)
	b.WriteString(`<div style="display:flex;gap:10px"><input type="text" name="users" value="` + esc(ownerVal) + `" placeholder="2849446053" style="flex:1"><button class="primary" type="submit">保存</button></div>`)
	b.WriteString(`</form></div>`)

	b.WriteString(`<table><thead><tr><th style="width:300px">插件</th><th style="width:70px">命令</th><th style="width:60px">前缀</th><th style="width:50px">@</th><th>允许的 QQ</th><th>允许的群</th><th style="width:100px">角色</th><th style="width:64px"></th></tr></thead><tbody>`)

	// 没有命令的插件（服务插件、纯消息插件）不需要命令权限，列出来只会干扰
	for _, man := range sortedManifests(d) {
		if len(man.Commands) == 0 {
			continue
		}
		users, groups, role, prefix, mention := effectiveAccess(d, man)

		prefixChecked, mentionChecked := "", ""
		if prefix {
			prefixChecked = " checked"
		}
		if mention {
			mentionChecked = " checked"
		}

		cmdLabel := fmt.Sprintf("%d 条", len(man.Commands))

		fmt.Fprintf(&b, `<tr><td><strong>%s</strong><br><code>%s</code><br><span class="muted">%s</span></td>
<td class="muted">%s</td>
<td><input type="checkbox" name="require_prefix" form="p_%s"%s title="必须带命令前缀"></td>
<td><input type="checkbox" name="require_mention" form="p_%s"%s title="群里必须 @ 机器人"></td>
<td><div style="display:flex;gap:4px"><input type="text" name="users" form="p_%s" value="%s" placeholder="不限" style="flex:1"><button type="button" data-owner="%s" data-for="p_%s" onclick="cwFillOwner(this)" title="填入主人 QQ" style="padding:3px 7px;font-size:11px">仅主人</button></div></td>
<td><input type="text" name="groups" form="p_%s" value="%s" placeholder="不限"></td>
<td><select name="role" form="p_%s"><option value="">不限</option><option value="admin"%s>群管理</option><option value="owner"%s>群主</option></select></td>
<td><form id="p_%s" method="post" action="/api/permissions"><input type="hidden" name="plugin" value="%s"><button>保存</button></form></td></tr>`,
			esc(man.Name), esc(man.ID), esc(man.Description),
			cmdLabel,
			esc(man.ID), prefixChecked,
			esc(man.ID), mentionChecked,
			esc(man.ID), esc(users), strconv.FormatInt(ownerQQ, 10), esc(man.ID),
			esc(man.ID), esc(groups),
			esc(man.ID), sel(role, "admin"), sel(role, "owner"),
			esc(man.ID), esc(man.ID))
	}
	b.WriteString(`</tbody></table>`)

	// 把没列出来的插件说明一下，免得以为漏了
	var idle []string
	for _, man := range sortedManifests(d) {
		if len(man.Commands) == 0 {
			name := man.Name
			if name == "" {
				name = man.ID
			}
			idle = append(idle, fmt.Sprintf("%s（%s）", esc(name), esc(man.ID)))
		}
	}
	if len(idle) > 0 {
		fmt.Fprintf(&b, `<p class="sub" style="margin-top:14px">以下插件没有命令，不需要权限控制：%s</p>`, strings.Join(idle, "、"))
	}

	if r.URL.Query().Get("saved") == "1" {
		b.WriteString(`<div class="notice">已保存，下一次消息就按新策略判定。</div>`)
	}

	// "仅主人"按钮：把主人 QQ 填进这个插件的允许列表
	b.WriteString(`<script>
function cwFillOwner(btn) {
  var formId = btn.dataset.for;
  var input = document.querySelector('input[name="users"][form="' + formId + '"]');
  if (!input) return;
  var qq = btn.dataset.owner;
  var cur = input.value.trim();
  if (!cur) { input.value = qq; return; }
  var parts = cur.split(/[|,，\s]+/).filter(Boolean);
  if (parts.indexOf(qq) === -1) { parts.push(qq); input.value = parts.join('|'); }
}
</script>`)

	s.render(w, "权限", "/permissions", b.String())
}

// sortedManifests 按 id 排序返回所有插件清单。
func sortedManifests(d *Deps) []*plugin.Manifest {
	all := d.Mgr.Manifests()
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*plugin.Manifest, 0, len(ids))
	for _, id := range ids {
		out = append(out, all[id])
	}
	return out
}

// effectiveAccess 返回插件最终生效的访问配置。
func effectiveAccess(d *Deps, man *plugin.Manifest) (users, groups, role string, prefix, mention bool) {
	u, g, r := man.Access.AllowUsers, man.Access.AllowGroups, man.Access.RequireRole
	prefix = boolOrDefault(man.Access.RequirePrefix, true)
	mention = boolOrDefault(man.Access.RequireMention, false)

	if d.Overrides != nil {
		if ov, ok := d.Overrides.Policy(man.ID); ok {
			u, g, r = ov.AllowUsers, ov.AllowGroups, ov.RequireRole
			if ov.RequirePrefix != nil {
				prefix = *ov.RequirePrefix
			}
			if ov.RequireMention != nil {
				mention = *ov.RequireMention
			}
		}
	}
	return joinInts(u), joinInts(g), r, prefix, mention
}

func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func sel(current, value string) string {
	if current == value {
		return " selected"
	}
	return ""
}

func (s *Server) pagePlugins(w http.ResponseWriter, r *http.Request) {
	d := s.Deps
	var b strings.Builder
	b.WriteString(`<h1>插件</h1><p class="sub">启用/禁用会立即启动或停止子进程；删除会移除插件目录（data/ 备份到 data/removed-plugins/）</p>`)

	b.WriteString(`<table><thead><tr><th>ID</th><th>名称</th><th>版本</th><th style="width:80px">优先级</th><th>状态</th><th>错误</th><th style="width:230px">操作</th></tr></thead><tbody>`)
	for _, p := range d.Mgr.List() {
		actions := ""
		switch p.State {
		case "ready":
			actions = btn(p.ID, "reload", "重载", "") + btn(p.ID, "disable", "禁用", "danger")
		case "disabled":
			actions = btn(p.ID, "enable", "启用", "")
		default:
			actions = btn(p.ID, "reload", "重试", "") + btn(p.ID, "disable", "禁用", "danger")
		}
		// 统一设置页对所有插件都可用（设置表单 + 指令 + 工具 + 可选扩展片段）
		actions += fmt.Sprintf(`<a href="/plugin/%s/" target="_blank" style="display:inline-block;margin-left:4px"><button>设置</button></a>`, esc(p.ID))
		actions += fmt.Sprintf(`<form method="post" action="/api/plugins/%s/delete" class="js-delete" style="display:inline" data-name="%s"><button class="danger">删除</button></form>`, esc(p.ID), esc(p.ID))

		// 优先级：管理员可在界面上改，数字大的先处理
		prio := 0
		if d.Mgr.PriorityOf != nil {
			if v, ok := d.Mgr.PriorityOf(p.ID); ok {
				prio = v
			}
		}
		prioCell := fmt.Sprintf(
			`<form method="post" action="/api/priority" style="display:flex;gap:4px"><input type="hidden" name="plugin" value="%s"><input type="number" name="priority" value="%d" min="-100" max="100" style="width:58px;padding:3px 5px"><button style="padding:3px 7px">存</button></form>`,
			esc(p.ID), prio,
		)

		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td class="muted">%s</td><td>%s</td><td>%s</td><td class="err">%s</td><td class="actions">%s</td></tr>`,
			esc(p.ID), esc(p.Name), esc(p.Version), prioCell, stateTag(p), esc(p.Error), actions)
	}
	b.WriteString(`</tbody></table>`)

	b.WriteString(`<h2>上传插件</h2>
<div class="form-box"><form method="post" action="/api/plugins/upload" enctype="multipart/form-data">
<label>选择 .anzu 包（用 qixinpack pack 生成，包内含 manifest.toml）</label>
<input type="file" name="file" accept=".anzu" required>
<div style="margin-top:16px"><button class="primary" type="submit">上传并加载</button></div>
</form></div>`)

	// 可信签名者：热更新列表，不用改 config 重启
	s.renderTrustedKeys(&b)

	if msg := r.URL.Query().Get("msg"); msg != "" {
		cls := "notice"
		if r.URL.Query().Get("ok") != "1" {
			cls = "notice"
		}
		fmt.Fprintf(&b, `<div class="%s">%s</div>`, cls, esc(msg))
	}

	s.render(w, "插件", "/plugins", b.String())
}

func btn(id, action, label, class string) string {
	return fmt.Sprintf(`<form method="post" action="/api/plugins/%s/%s" style="display:inline"><button class="%s">%s</button></form>`,
		esc(id), esc(action), class, esc(label))
}

func (s *Server) pageHelpSettings(w http.ResponseWriter, r *http.Request) {
	d := s.Deps
	var b strings.Builder
	b.WriteString(`<h1>指令显示</h1><p class="sub">控制哪些插件和命令出现在 /help。标着「管理员专属」的即使勾选，普通群员也看不到（由权限自动过滤）</p>`)

	byPlugin := make(map[string][]*registry.Feature)
	var order []string
	for _, f := range d.Reg.All() {
		if f.Kind != registry.KindCommand {
			continue
		}
		if _, ok := byPlugin[f.Plugin]; !ok {
			order = append(order, f.Plugin)
		}
		byPlugin[f.Plugin] = append(byPlugin[f.Plugin], f)
	}

	// 插件 id → 中文名
	names := make(map[string]string, len(order))
	if d.Mgr != nil {
		for _, st := range d.Mgr.List() {
			if st.Name != "" {
				names[st.ID] = st.Name
			}
		}
	}

	b.WriteString(`<div class="row" style="margin-bottom:10px">
<button type="button" onclick="hgAll(true)">全部展开</button>
<button type="button" onclick="hgAll(false)">全部收起</button>
</div><form method="post" action="/api/help">`)

	for _, plugin := range order {
		show := true
		if d.Overrides != nil {
			show = d.Overrides.PluginVisible(plugin)
		}
		checked := ""
		if show {
			checked = " checked"
		}

		cmds := byPlugin[plugin]
		label := names[plugin]
		// 有中文名就把它放前面，id 退成灰色小字；没有就只用 id
		title := `<strong>` + esc(plugin) + `</strong>`
		if label != "" {
			title = `<strong>` + esc(label) + `</strong> <code class="id">` + esc(plugin) + `</code>`
		}

		fmt.Fprintf(&b, `<details class="hg">
<summary><span class="arrow">▶</span>
<input type="checkbox" name="p:%s"%s onclick="event.stopPropagation()">
%s <span class="tag">%d</span>
</summary><div class="body"><table><tbody>`,
			esc(plugin), checked, title, len(cmds))

		for _, f := range cmds {
			hidden := f.Hidden
			if d.Overrides != nil {
				if h := d.Overrides.FeatureHidden(f.ID); h != nil {
					hidden = *h
				}
			}
			fchecked := ""
			if !hidden {
				fchecked = " checked"
			}
			adminOnly := ""
			if f.Policy.RequireRole != "" || len(f.Policy.AllowUsers) > 0 {
				adminOnly = ` <span class="tag">管理员</span>`
			}
			fmt.Fprintf(&b, `<tr><td style="width:52px"><input type="checkbox" name="f:%s"%s></td><td><code>%s</code>%s</td><td class="muted">%s</td></tr>`,
				esc(f.ID), fchecked, esc(f.Usage), adminOnly, esc(f.Description))
		}
		b.WriteString(`</tbody></table></div></details>`)
	}

	b.WriteString(`<div style="margin-top:14px"><button class="primary" type="submit">保存</button></div></form>
<script>
function hgAll(open){
  document.querySelectorAll('details.hg').forEach(function(d){ d.open = open });
}
</script>`)

	if r.URL.Query().Get("saved") == "1" {
		b.WriteString(`<div class="notice">已保存，下一条 /help 就会按新配置显示。</div>`)
	}

	s.render(w, "指令", "/help-settings", b.String())
}

func (s *Server) pageLogs(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(`<h1>日志</h1><p class="sub">最近 1000 条，自动刷新</p>
<div class="row" style="margin-bottom:10px">
<select id="level" style="width:160px"><option value="">全部级别</option><option>INFO</option><option>WARN</option><option>ERROR</option><option>DEBUG</option></select>
<label style="margin:0"><input type="checkbox" id="follow" checked> 自动滚动</label>
</div>
<div class="logbox" id="log"></div>
<script>
let last = 0;
const box = document.getElementById('log');
const lv = document.getElementById('level');
const follow = document.getElementById('follow');

function line(e) {
  const div = document.createElement('div');
  div.className = 'lv-' + e.level;
  div.textContent = e.time + ' ' + e.level.padEnd(5) + ' ' + e.msg + (e.attrs ? '  ' + e.attrs : '');
  div.dataset.level = e.level;
  return div;
}
function applyFilter() {
  const want = lv.value;
  for (const el of box.children) {
    el.style.display = (!want || el.dataset.level === want) ? '' : 'none';
  }
}
async function poll() {
  try {
    const r = await fetch('/api/logs?after=' + last);
    if (!r.ok) return;
    const data = await r.json();
    for (const e of data.items) { box.appendChild(line(e)); }
    last = data.last;
    while (box.children.length > 2000) box.removeChild(box.firstChild);
    applyFilter();
    if (follow.checked) box.scrollTop = box.scrollHeight;
  } catch (e) {}
}
lv.addEventListener('change', applyFilter);
poll();
setInterval(poll, 2000);
</script>`)
	s.render(w, "日志", "/logs", b.String())
}

func joinInts(v []int64) string {
	if len(v) == 0 {
		return ""
	}
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return strings.Join(parts, ",")
}

// renderTrustedKeys 画"可信签名者"这块。
//
// 这是"别人怎么签名插件"的入口：对方发来包 + 公钥，
// 你把公钥贴进来，立刻就能装他的插件，不用改 config 重启。
func (s *Server) renderTrustedKeys(b *strings.Builder) {
	d := s.Deps
	if d.Trust == nil || d.Inst == nil {
		return
	}

	b.WriteString(`<h2>可信签名者</h2>
<p class="sub">官方插件用内置公钥验签，**你什么都不用配**。第三方给你发的插件，把他的公钥贴在这里就能装（只影响你这台，立刻生效、不用重启）。</p>
<table><thead><tr><th style="width:120px">指纹</th><th>公钥</th><th style="width:90px">来源</th><th style="width:90px"></th></tr></thead><tbody>`)

	for _, k := range d.Trust.All() {
		fp := pluginFingerprint(k)
		src := d.Trust.Source(k)
		label := "控制台"
		action := fmt.Sprintf(
			`<form method="post" action="/api/trusted-keys/delete" onsubmit="return confirm('移除后，用它签的包将无法安装')"><input type="hidden" name="key" value="%s"><button class="danger">移除</button></form>`,
			esc(k))
		switch src {
		case "official":
			label = "框架内置（官方）"
			action = `<span class="muted">随框架版本</span>`
		case "config":
			label = "config.toml"
			action = `<span class="muted">改配置文件</span>`
		}
		fmt.Fprintf(b, `<tr><td><code>%s</code></td><td class="muted" style="word-break:break-all;font-size:12px">%s</td><td>%s</td><td class="actions">%s</td></tr>`,
			esc(fp), esc(k), esc(label), action)
	}
	if len(d.Trust.All()) == 0 {
		b.WriteString(`<tr><td colspan="4" class="muted">还没有配置任何公钥——当前**不校验签名**（开发模式）</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)

	b.WriteString(`<div class="form-box"><form method="post" action="/api/trusted-keys">
<label>添加公钥（直接贴 base64；粘 <code>trusted_keys = ["..."]</code> 整行也认）</label>
<input type="text" name="key" placeholder="UkVeBEDsEKbcwm+2bsVgk1zhwj78z74x5ygnG2E7+9I=" required>
<p class="sub" style="margin:8px 0 0">对方可以用 <code>qixinpack check 包.anzu -pub 公钥</code> 看到自己的指纹，和你这里显示的应该一致。</p>
<div style="margin-top:16px"><button class="primary" type="submit">添加并生效</button></div>
</form></div>`)
}
