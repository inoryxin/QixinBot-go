package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xin/onebot/internal/config"
	"github.com/xin/onebot/internal/metrics"
	"github.com/xin/onebot/internal/override"
	"github.com/xin/onebot/internal/plugin"
	"github.com/xin/onebot/internal/registry"
)

// apiSaveConnect 保存 OneBot 连接配置。需要重启生效。
func (s *Server) apiSaveConnect(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d := s.Deps
	if len(d.Config.Accounts) == 0 {
		http.Error(w, "没有配置账号", http.StatusInternalServerError)
		return
	}
	a := &d.Config.Accounts[0]
	a.WSURL = strings.TrimSpace(r.FormValue("ws_url"))
	a.AccessToken = strings.TrimSpace(r.FormValue("access_token"))
	a.HTTPURL = strings.TrimSpace(r.FormValue("http_url"))
	a.HTTPToken = strings.TrimSpace(r.FormValue("http_token"))

	if a.WSURL == "" {
		http.Error(w, "ws_url 不能为空", http.StatusBadRequest)
		return
	}

	path := d.ConfigPath
	if path == "" {
		path = "config.toml"
	}
	if err := config.Save(path, d.Config); err != nil {
		s.Log.Error("保存配置失败", "err", err)
		http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Log.Info("连接配置已更新（重启后生效）", "ws_url", a.WSURL)
	http.Redirect(w, r, "/connect?saved=1", http.StatusSeeOther)
}

// apiSavePermissions 保存单条命令的权限覆盖。
// apiSavePriority 设置插件分发优先级（数字大的先处理）。
func (s *Server) apiSavePriority(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pluginID := r.FormValue("plugin")
	if pluginID == "" {
		http.Error(w, "缺少 plugin", http.StatusBadRequest)
		return
	}
	if s.Deps.Overrides == nil {
		http.Error(w, "覆盖存储未启用", http.StatusInternalServerError)
		return
	}
	value, err := strconv.Atoi(strings.TrimSpace(r.FormValue("priority")))
	if err != nil {
		http.Error(w, "优先级必须是整数（-100 到 100）", http.StatusBadRequest)
		return
	}
	if value < -100 || value > 100 {
		http.Error(w, "优先级范围是 -100 到 100", http.StatusBadRequest)
		return
	}
	s.Deps.Overrides.SetPriority(pluginID, value)
	if r.Header.Get("Accept") == "application/json" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "priority": value})
		return
	}
	http.Redirect(w, r, "/plugins?msg=优先级已保存", http.StatusSeeOther)
}

func (s *Server) apiSavePermissions(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// 兼容新旧两种字段名：新的是 plugin，旧的是 feature
	pluginID := r.FormValue("plugin")
	if pluginID == "" {
		pluginID = r.FormValue("feature")
	}
	if pluginID == "" {
		http.Error(w, "缺少 plugin", http.StatusBadRequest)
		return
	}
	if s.Deps.Overrides == nil {
		http.Error(w, "覆盖存储未启用", http.StatusInternalServerError)
		return
	}

	// checkbox 未勾选时不会出现在表单里，用 Has 判断
	requirePrefix := r.Form.Has("require_prefix")
	requireMention := r.Form.Has("require_mention")

	p := override.Policy{
		AllowUsers:     parseIntList(r.FormValue("users")),
		AllowGroups:    parseIntList(r.FormValue("groups")),
		RequireRole:    strings.TrimSpace(r.FormValue("role")),
		RequirePrefix:  &requirePrefix,
		RequireMention: &requireMention,
	}
	s.Deps.Overrides.SetPolicy(pluginID, p)
	s.Log.Info("权限已更新", "plugin", pluginID,
		"users", p.AllowUsers, "groups", p.AllowGroups, "role", p.RequireRole)
	http.Redirect(w, r, "/permissions?saved=1", http.StatusSeeOther)
}

// apiSaveHelp 保存 /help 的显示开关。
func (s *Server) apiSaveHelp(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Deps.Overrides == nil {
		http.Error(w, "覆盖存储未启用", http.StatusInternalServerError)
		return
	}

	// 插件级：表单里没出现的 checkbox 表示未勾选
	plugins := map[string]bool{}
	for _, f := range s.Deps.Reg.All() {
		plugins[f.Plugin] = true
	}
	for plugin := range plugins {
		show := r.FormValue("p:"+plugin) == "on"
		s.Deps.Overrides.SetPluginVisible(plugin, show)
	}

	// 命令级
	for _, f := range s.Deps.Reg.All() {
		if f.Kind != registry.KindCommand {
			continue
		}
		visible := r.FormValue("f:"+f.ID) == "on"
		hidden := !visible
		s.Deps.Overrides.SetFeatureHidden(f.ID, &hidden)
	}

	s.Log.Info("指令显示配置已更新")
	http.Redirect(w, r, "/help-settings?saved=1", http.StatusSeeOther)
}

// apiPluginAction 处理 reload / enable / disable / delete。
func (s *Server) apiPluginAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.PathValue("action")
	// delete 会拿 id 去 Join 目录再删，畸形 id（如 ..%5C..%5Cx）不能放行
	if !validPluginPathID(id) {
		http.Error(w, "插件不存在", http.StatusNotFound)
		return
	}

	var err error
	switch action {
	case "reload":
		err = s.Deps.Mgr.Reload(r.Context(), id)
	case "enable":
		err = s.Deps.Mgr.Enable(r.Context(), id)
	case "disable":
		err = s.Deps.Mgr.Disable(id)
	case "delete":
		err = s.Deps.Mgr.Remove(id)
	default:
		http.Error(w, "未知操作: "+action, http.StatusNotFound)
		return
	}

	if err != nil {
		s.Log.Warn("插件操作失败", "id", id, "action", action, "err", err)
		http.Redirect(w, r, "/plugins?ok=0&msg="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	s.Log.Info("插件操作完成", "id", id, "action", action)
	http.Redirect(w, r, "/plugins?ok=1&msg="+urlQuery(fmt.Sprintf("%s: %s 完成", id, action)), http.StatusSeeOther)
}

// apiUploadPlugin 接收 zip 并安装。
func (s *Server) apiUploadPlugin(w http.ResponseWriter, r *http.Request) {
	// 64MB 上限，和插件包大小约定一致
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		http.Redirect(w, r, "/plugins?ok=0&msg="+urlQuery("读取上传失败: "+err.Error()), http.StatusSeeOther)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/plugins?ok=0&msg="+urlQuery("没有选择文件"), http.StatusSeeOther)
		return
	}
	defer file.Close()

	tmp, err := io.ReadAll(io.LimitReader(file, 256<<20))
	if err != nil {
		http.Redirect(w, r, "/plugins?ok=0&msg="+urlQuery("读取失败"), http.StatusSeeOther)
		return
	}

	kind, _ := plugin.ZipKind(tmp)
	if kind == "runtime" {
		if s.Deps.Mgr.Inst != nil {
			if verr := s.Deps.Mgr.Inst.VerifyPackage(tmp); verr != nil {
				http.Redirect(w, r, "/deps?err="+urlQuery("运行时签名校验失败: "+verr.Error()), http.StatusSeeOther)
				return
			}
		}
		info, rerr := s.Deps.Mgr.InstallRuntimeZip(tmp)
		if rerr != nil {
			http.Redirect(w, r, "/deps?err="+urlQuery("运行时安装失败: "+rerr.Error()), http.StatusSeeOther)
			return
		}
		msg := fmt.Sprintf("已安装运行时 %s %s（%s）到 %s", info.Kind, info.Version, info.Platform, info.Path)
		if info.Current {
			msg += "，已设为当前运行时"
		}
		http.Redirect(w, r, "/deps?msg="+urlQuery(msg), http.StatusSeeOther)
		return
	}
	id, err := s.Deps.Mgr.InstallZip(bytes.NewReader(tmp), int64(len(tmp)))
	if err != nil {
		s.Log.Warn("插件安装失败", "file", header.Filename, "err", err)
		http.Redirect(w, r, "/plugins?ok=0&msg="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}

	// 立刻加载，不用等热重载
	if err := s.Deps.Mgr.Reload(r.Context(), id); err != nil {
		// 热插拔监视可能抢先把它登记成"新插件默认禁用"；
		// 上传语义是"装上就用"，这里补一次启用再加载。
		if eerr := s.Deps.Mgr.Enable(r.Context(), id); eerr == nil {
			if err2 := s.Deps.Mgr.Reload(r.Context(), id); err2 == nil {
				goto loaded
			} else {
				err = err2
			}
		}
		http.Redirect(w, r, "/plugins?ok=0&msg="+urlQuery(fmt.Sprintf("已解压但加载失败: %v", err)), http.StatusSeeOther)
		return
	}
loaded:
	if st := s.Deps.Mgr.StateOf(id); st != "" && st != "ready" {
		s.Log.Info("插件已安装但未运行", "plugin", id, "state", st)
		http.Redirect(w, r, "/plugins?ok=1&msg="+urlQuery(
			fmt.Sprintf("已安装，但未启动（状态: %s；可能是平台不匹配或清单禁用）", st)), http.StatusSeeOther)
		return
	}
	s.Log.Info("插件已安装并加载", "plugin", id, "file", header.Filename)
	http.Redirect(w, r, "/plugins?ok=1&msg="+urlQuery("已安装并加载: "+id), http.StatusSeeOther)
}

// apiLogs 返回增量日志。
func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Logs == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "last": 0})
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	items := s.Deps.Logs.Since(after, 200)
	// last = 本批最后一项的 seq（风暴期间 200 条装不下时，
	// 下次从断点继续；用 LastSeq 会把没取到的行永久跳过）
	last := after
	if len(items) > 0 {
		last = items[len(items)-1].Seq
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"items": items,
		"last":  last,
	})
}

// apiOverview 返回 JSON 概览，方便脚本或监控。
func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	type accountInfo struct {
		ID        string `json:"id"`
		WSURL     string `json:"ws_url"`
		Connected bool   `json:"connected"`
		SelfID    int64  `json:"self_id"`
		Nickname  string `json:"nickname"`
	}
	d := s.Deps
	accounts := make([]accountInfo, 0, len(d.Config.Accounts))
	for _, a := range d.Config.Accounts {
		info := accountInfo{ID: a.ID, WSURL: a.WSURL}
		if c := d.Clients[a.ID]; c != nil {
			info.Connected = c.Connected()
			info.SelfID = c.SelfID()
			info.Nickname = c.Nickname()
		}
		accounts = append(accounts, info)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uptime_seconds": int64(time.Since(d.StartedAt).Seconds()),
		"version":        d.Version,
		"accounts":       accounts,
		"plugins":        d.Mgr.List(),
		"features":       len(d.Reg.All()),
	})
}

func parseIntList(s string) []int64 {
	// 支持逗号、竖线、空白三种分隔
	s = strings.NewReplacer("|", ",", "|", ",", " ", ",").Replace(s)
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.ParseInt(part, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func urlQuery(s string) string { return url.QueryEscape(s) }

// apiRuntimePaths 保存管理员指定的解释器路径（data/runtime.json）。
// 用户可能装了多个 Node/Python，想固定用某个。
func (s *Server) apiRuntimePaths(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	path := strings.TrimSpace(r.FormValue("path"))
	if kind == "" {
		http.Error(w, "需要 kind", http.StatusBadRequest)
		return
	}

	file := filepath.Join(filepath.Dir(s.Deps.RuntimeDir), "runtime.json")
	cur := map[string]string{}
	if raw, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(raw, &cur)
	}
	if path == "" {
		delete(cur, kind)
	} else {
		cur[kind] = path
	}
	raw, _ := json.MarshalIndent(cur, "", "  ")
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	plugin.SetRuntimeOverrides(cur)

	// 同时把路径交给插件：QIXIN_NODE / QIXIN_PYTHON
	if s.Deps.Mgr != nil {
		if v := cur["node"]; v != "" {
			s.Deps.Mgr.SetEnv("QIXIN_NODE", v)
		} else {
			s.Deps.Mgr.DeleteEnv("QIXIN_NODE")
		}
		if v := cur["python"]; v != "" {
			s.Deps.Mgr.SetEnv("QIXIN_PYTHON", v)
		} else {
			s.Deps.Mgr.DeleteEnv("QIXIN_PYTHON")
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// apiMetrics 返回 24 小时指标时间序列（5 分钟一窗）。
// GET /api/metrics
func (s *Server) apiMetrics(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{"windows": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window_sec": metrics.WindowSec,
		"windows":    s.Deps.Metrics.Snapshot(),
	})
}
