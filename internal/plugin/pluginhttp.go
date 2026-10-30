package plugin

// 插件 HTTP 面板代理。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PluginHTTPResponse 是插件自定义页面接口的返回。
type PluginHTTPResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

// PluginHTTP 把 WebUI 的 /api/plugin/<id>/http/* 转给插件处理，
// 让插件面板可以上传文件、管理自己的数据。

// PluginHTTP 把 WebUI 的 /api/plugin/<id>/http/* 转给插件处理，
// 让插件面板可以上传文件、管理自己的数据。
func (m *Manager) PluginHTTP(ctx context.Context, id, method, path, query, contentType string, body []byte) (*PluginHTTPResponse, error) {
	cur, release, err := m.acquireHost(ctx, id)
	if err != nil || cur == nil {
		return nil, fmt.Errorf("插件 %s 不可用: %w", id, err)
	}
	defer release()
	unlock := m.lockFor(cur, nil)
	defer unlock()

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := cur.Call(callCtx, "plugin.http", map[string]any{
		"method":       method,
		"path":         path,
		"query":        query,
		"content_type": contentType,
		"body":         base64.StdEncoding.EncodeToString(body),
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Status      int    `json:"status"`
		ContentType string `json:"content_type"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("插件返回格式不对: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(out.Body)
	if err != nil {
		return nil, fmt.Errorf("插件返回体解码失败: %w", err)
	}
	if out.Status == 0 {
		out.Status = http.StatusOK
	}
	if out.ContentType == "" {
		out.ContentType = "application/octet-stream"
	}
	return &PluginHTTPResponse{Status: out.Status, ContentType: out.ContentType, Body: raw}, nil
}

// CallTool 按名字找到提供者并执行。插件互调（host.call_capability）也走这里。
