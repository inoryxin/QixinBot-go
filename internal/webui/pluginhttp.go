package webui

import (
	"io"
	"net/http"
	"strings"
)

// 插件 HTTP 代理的请求体上限。
// 请求还要 base64 塞进 JSON-RPC，而宿主单行上限是 16MiB；8MiB 原文件
// 编码后约 10.7MiB，留出 JSON/字段开销后仍能安全通过。
const maxPluginHTTPBody = 8 << 20

// handlePluginHTTP 把插件面板的请求转给插件自己的 plugin.http 处理。
// GET 用于列表/图片，POST 用于上传/删除（已被 guard 的 CSRF 保护）。
func (s *Server) handlePluginHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPluginPathID(id) {
		http.Error(w, "插件不存在", http.StatusNotFound)
		return
	}
	prefix := "/api/plugin/" + id + "/http"
	path := strings.TrimPrefix(r.URL.Path, prefix)
	if path == "" {
		path = "/"
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxPluginHTTPBody+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(b) > maxPluginHTTPBody {
			http.Error(w, "请求体超过 8MiB，请分片上传或改用插件自己的上传接口", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
	}
	resp, err := s.Deps.Mgr.PluginHTTP(r.Context(), id, r.Method, path, r.URL.RawQuery,
		r.Header.Get("Content-Type"), body)
	if err != nil {
		http.Error(w, "插件接口失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
