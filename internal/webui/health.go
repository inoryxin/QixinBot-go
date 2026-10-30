package webui

// 就绪探针：给 systemd / 容器 / 监控用，区分“进程活着”和“能干活”。
// /healthz 仍然只表示进程存活；/readyz 检查账号连接和插件状态。
//
// /readyz 不在登录鉴权后面（探针机器没有凭据），所以**只回计数、
// 不回具体的插件 id/状态**——内网部署也一样，别把内部清单泄露给扫描器。
// 具体是哪个插件坏了，看 WebUI 插件页或 /api/overview。

import (
	"net/http"
	"time"
)

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	d := s.Deps
	connected, accounts := 0, 0
	for _, c := range d.Clients {
		accounts++
		if c != nil && c.Connected() {
			connected++
		}
	}

	pluginsTotal, pluginsBad := 0, 0
	if d.Mgr != nil {
		for _, st := range d.Mgr.List() {
			pluginsTotal++
			if st.State != "ready" && st.State != "disabled" {
				pluginsBad++
			}
		}
	}

	status := "ok"
	if accounts == 0 || connected == 0 {
		status = "unready"
	} else if connected < accounts || pluginsBad > 0 {
		status = "degraded"
	}
	code := http.StatusOK
	if status == "unready" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":      status,
		"version":     d.Version,
		"uptime_sec":  int64(time.Since(d.StartedAt).Seconds()),
		"accounts":    accounts,
		"connected":   connected,
		"plugins":     pluginsTotal,
		"plugins_bad": pluginsBad,
	})
}
