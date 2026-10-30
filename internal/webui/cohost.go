package webui

// cohost 池管理页：池生命周期、运行时状态、插件归属。

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xin/onebot/internal/plugin"
)

func (s *Server) pageCohost(w http.ResponseWriter, r *http.Request) {
	d := s.Deps
	var b strings.Builder
	b.WriteString(`<h1>共享池</h1>
<p class="sub">池按成员上限启动独立 multi-host；未分配/已逐出/池熔断的插件走独立进程。修改归属会重载该插件。</p>`)
	writeCohostNotice(&b, r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
	if d.Mgr == nil || d.Mgr.Pools == nil {
		b.WriteString(`<p class="muted">池存储不可用（见启动日志）。</p>`)
		s.render(w, "共享池", "/cohost", b.String())
		return
	}

	b.WriteString(`<h2>新建池</h2><div class="form-box"><form method="post" action="/api/cohost/pools">
<label>池名</label><input name="name" required placeholder="例如：基础 / 娱乐 / 个人">
<label>成员上限</label><input name="max_members" type="number" min="1" max="64" value="12">
<div style="margin-top:14px"><button class="primary">创建池</button></div></form></div>`)

	pools, assignments := d.Mgr.Pools.List()
	s.renderCohostPoolTable(&b, pools, assignments)
	s.renderCohostPluginTable(&b, pools, assignments)
	renderCohostCrashTable(&b, pools, assignments)
	s.render(w, "共享池", "/cohost", b.String())
}

func writeCohostNotice(b *strings.Builder, msg, errMsg string) {
	if msg != "" {
		fmt.Fprintf(b, `<p class="sub">%s</p>`, esc(msg))
	}
	if errMsg != "" {
		fmt.Fprintf(b, `<p class="sub" style="color:var(--bad)">%s</p>`, esc(errMsg))
	}
}

func (s *Server) renderCohostPoolTable(b *strings.Builder, pools []plugin.CohostPool, assignments map[string]plugin.CohostAssignment) {
	b.WriteString(`<h2>池列表</h2><table><thead><tr><th>池名</th><th>成员</th><th>上限</th><th>进程</th><th>内存</th><th>状态</th><th style="width:300px">操作</th></tr></thead><tbody>`)
	if len(pools) == 0 {
		b.WriteString(`<tr><td colspan="7" class="muted">还没有池；先建一个。</td></tr>`)
	}
	for _, p := range pools {
		n := 0
		for _, a := range assignments {
			if a.PoolID == p.ID && a.State == plugin.CohostStateShared {
				n++
			}
		}
		rt := s.Deps.Mgr.CohostPoolRuntime(p.ID)
		pid, mem, health := "—", "—", "未启动"
		if rt.Running {
			pid = strconv.Itoa(rt.PID)
			mem = fmt.Sprintf("%.0f MB", rt.RSSMB)
			health = "正常"
		}
		if rt.Degraded {
			health = "熔断"
		}
		fmt.Fprintf(b, `<tr><td><strong>%s</strong></td><td>%d</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td>
<td><form method="post" action="/api/cohost/pools/update" style="display:flex;gap:4px;align-items:center;margin:0 0 4px">
<input type="hidden" name="id" value="%s"><input name="name" value="%s" style="width:92px;min-width:0" title="池名">
<input name="max_members" type="number" min="1" max="64" value="%d" style="width:58px;min-width:0" title="上限"><button>保存</button></form>
<form method="post" action="/api/cohost/pools/delete" class="js-delete" style="display:inline" data-name="%s"><input type="hidden" name="id" value="%s"><button class="danger">删除</button></form></td></tr>`,
			esc(p.Name), n, p.MaxMembers, pid, mem, health, esc(p.ID), esc(p.Name), p.MaxMembers, esc(p.Name), esc(p.ID))
	}
	b.WriteString(`</tbody></table>`)
}

func (s *Server) renderCohostPluginTable(b *strings.Builder, pools []plugin.CohostPool, assignments map[string]plugin.CohostAssignment) {
	b.WriteString(`<h2>插件归属</h2><p class="sub">未分配的插件默认 quarantine（独立进程）；新建插件也默认不分配。</p>
<table><thead><tr><th>插件</th><th>状态</th><th>当前池</th><th style="width:320px">分配</th></tr></thead><tbody>`)
	for _, st := range s.Deps.Mgr.List() {
		current := st.CohostPool
		if current == "" {
			current = "quarantine"
		}
		fmt.Fprintf(b, `<tr><td><strong>%s</strong><br><span class="muted">%s</span></td><td>%s</td><td>%s`, esc(st.ID), esc(st.Name), stateTag(st), esc(current))
		if st.CohostCrashes > 0 {
			fmt.Fprintf(b, ` <span class="muted">崩 %d</span>`, st.CohostCrashes)
		}
		b.WriteString(`</td><td><form method="post" action="/api/cohost/assign" style="display:flex;gap:6px"><input type="hidden" name="plugin_id" value="`)
		b.WriteString(esc(st.ID))
		b.WriteString(`"><select name="pool_id" style="min-width:150px"><option value="">quarantine（独立）</option>`)
		for _, p := range pools {
			sel := ""
			if a, ok := assignments[st.ID]; ok && a.PoolID == p.ID {
				sel = " selected"
			}
			fmt.Fprintf(b, `<option value="%s"%s>%s</option>`, esc(p.ID), sel, esc(p.Name))
		}
		b.WriteString(`</select><button>保存</button></form></td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
}

type cohostCrashEvent struct {
	At     int64
	Plugin string
	Pool   string
	State  string
}

func renderCohostCrashTable(b *strings.Builder, pools []plugin.CohostPool, assignments map[string]plugin.CohostAssignment) {
	poolName := make(map[string]string, len(pools))
	for _, p := range pools {
		poolName[p.ID] = p.Name
	}
	events := make([]cohostCrashEvent, 0)
	for id, a := range assignments {
		for _, at := range a.CrashTimes {
			events = append(events, cohostCrashEvent{At: at, Plugin: id, Pool: poolName[a.PoolID], State: a.State})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].At > events[j].At })
	if len(events) > 20 {
		events = events[:20]
	}
	b.WriteString(`<h2>最近崩溃</h2><p class="sub">24h 内共享池 worker 崩溃记录；已逐出插件的记录也保留。</p>
<table><thead><tr><th>时间</th><th>插件</th><th>池</th><th>状态</th></tr></thead><tbody>`)
	if len(events) == 0 {
		b.WriteString(`<tr><td colspan="4" class="muted">暂无记录。</td></tr>`)
	}
	for _, e := range events {
		pool := e.Pool
		if pool == "" {
			pool = "—"
		}
		fmt.Fprintf(b, `<tr><td>%s</td><td><code>%s</code></td><td>%s</td><td>%s</td></tr>`,
			time.Unix(e.At, 0).Format("01-02 15:04:05"), esc(e.Plugin), esc(pool), esc(e.State))
	}
	b.WriteString(`</tbody></table>`)
}

func (s *Server) apiCohostCreatePool(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Mgr == nil || s.Deps.Mgr.Pools == nil {
		http.Error(w, "池存储不可用", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	max, _ := strconv.Atoi(r.FormValue("max_members"))
	if _, err := s.Deps.Mgr.Pools.Create(r.FormValue("name"), max); err != nil {
		http.Redirect(w, r, "/cohost?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/cohost?msg=池已创建", http.StatusSeeOther)
}

func (s *Server) apiCohostDeletePool(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Mgr == nil || s.Deps.Mgr.Pools == nil {
		http.Error(w, "池存储不可用", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Deps.Mgr.Pools.Delete(r.FormValue("id")); err != nil {
		http.Redirect(w, r, "/cohost?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/cohost?msg=池已删除", http.StatusSeeOther)
}

func (s *Server) apiCohostUpdatePool(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Mgr == nil || s.Deps.Mgr.Pools == nil {
		http.Error(w, "池存储不可用", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	max, _ := strconv.Atoi(r.FormValue("max_members"))
	if err := s.Deps.Mgr.Pools.UpdatePool(r.FormValue("id"), r.FormValue("name"), max); err != nil {
		http.Redirect(w, r, "/cohost?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/cohost?msg=池配置已更新", http.StatusSeeOther)
}

// apiCohostPools 返回池、运行时和成员归属，供脚本/后续前端使用。
func (s *Server) apiCohostPools(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Mgr == nil || s.Deps.Mgr.Pools == nil {
		http.Error(w, "池存储不可用", http.StatusServiceUnavailable)
		return
	}
	pools, assignments := s.Deps.Mgr.Pools.List()
	type view struct {
		ID         string             `json:"id"`
		Name       string             `json:"name"`
		MaxMembers int                `json:"max_members"`
		Members    []string           `json:"members"`
		Running    bool               `json:"running"`
		PID        int                `json:"pid,omitempty"`
		RSSMB      float64            `json:"rss_mb,omitempty"`
		Degraded   bool               `json:"degraded"`
		Crashes    map[string][]int64 `json:"crashes,omitempty"`
	}
	out := make([]view, 0, len(pools))
	for _, p := range pools {
		rt := s.Deps.Mgr.CohostPoolRuntime(p.ID)
		v := view{ID: p.ID, Name: p.Name, MaxMembers: p.MaxMembers, Running: rt.Running, PID: rt.PID, RSSMB: rt.RSSMB, Degraded: rt.Degraded}
		for id, a := range assignments {
			if a.PoolID != p.ID {
				continue
			}
			if a.State == "shared" {
				v.Members = append(v.Members, id)
			}
			if len(a.CrashTimes) > 0 {
				if v.Crashes == nil {
					v.Crashes = make(map[string][]int64)
				}
				v.Crashes[id] = a.CrashTimes
			}
		}
		sort.Strings(v.Members)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

func (s *Server) apiCohostAssign(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Mgr == nil || s.Deps.Mgr.Pools == nil {
		http.Error(w, "池存储不可用", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pluginID := strings.TrimSpace(r.FormValue("plugin_id"))
	if _, ok := s.Deps.Mgr.Manifests()[pluginID]; !ok {
		http.Redirect(w, r, "/cohost?err="+urlQuery("插件不存在: "+pluginID), http.StatusSeeOther)
		return
	}
	if err := s.Deps.Mgr.Pools.Assign(pluginID, r.FormValue("pool_id")); err != nil {
		http.Redirect(w, r, "/cohost?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	// 人工恢复/移池：清零逐出计数，并立即按新归属重载，不需要重启整个框架。
	s.Deps.Mgr.ResetCohostEviction(pluginID)
	if err := s.Deps.Mgr.Reload(context.Background(), pluginID); err != nil {
		http.Redirect(w, r, "/cohost?err="+urlQuery("归属已保存，但重载失败: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/cohost?msg=归属已保存并已按新池重载", http.StatusSeeOther)
}
