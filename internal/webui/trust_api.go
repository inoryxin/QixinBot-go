package webui

import (
	"net/http"
	"strings"

	"github.com/xin/onebot/internal/plugin"
)

// 可信签名者的热更新。
//
// 场景：第三方发来一个插件包，签名者不是你。
// 你要做的不是重启改 config，而是在插件页把这串公钥贴进去——
// 立刻生效，下次安装就认了。

type trustedKeyView struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Source      string `json:"source"` // config | added
}

// apiTrustedKeys 列出当前信任的公钥。
func (s *Server) apiTrustedKeys(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Trust == nil {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}, "allow_unsigned": false})
		return
	}

	views := make([]trustedKeyView, 0, 8)
	for _, k := range s.Deps.Trust.All() {
		views = append(views, trustedKeyView{
			Key:         k,
			Fingerprint: pluginFingerprint(k),
			Source:      s.Deps.Trust.Source(k),
		})
	}

	allowUnsigned := false
	if s.Deps.Config != nil {
		allowUnsigned = s.Deps.Config.Plugin.AllowUnsigned
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":           views,
		"allow_unsigned": allowUnsigned,
	})
}

// apiAddTrustedKey 加一个公钥，立刻生效。
func (s *Server) apiAddTrustedKey(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Trust == nil || s.Deps.Inst == nil {
		writeErr(w, http.StatusInternalServerError, "信任列表未启用")
		return
	}

	key := strings.TrimSpace(r.FormValue("key"))
	// 允许直接粘整个 keygen 输出的提示行（"trusted_keys = [...]"）或带引号
	key = sanitizeKeyInput(key)

	if err := s.Deps.Trust.Add(key); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// **热更新**：立刻让 installer 用新列表
	if err := s.Deps.Inst.SetTrustedKeys(s.Deps.Trust.All()); err != nil {
		writeErr(w, http.StatusBadRequest, "公钥格式不对: "+err.Error())
		return
	}

	s.Log.Info("控制台添加可信公钥", "fingerprint", pluginFingerprint(key))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "fingerprint": pluginFingerprint(key)})
}

// apiRemoveTrustedKey 删一个运行期加的公钥。
func (s *Server) apiRemoveTrustedKey(w http.ResponseWriter, r *http.Request) {
	if s.Deps.Trust == nil || s.Deps.Inst == nil {
		writeErr(w, http.StatusInternalServerError, "信任列表未启用")
		return
	}

	key := sanitizeKeyInput(r.FormValue("key"))
	if err := s.Deps.Trust.Remove(key); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Deps.Inst.SetTrustedKeys(s.Deps.Trust.All()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.Log.Info("控制台移除可信公钥", "fingerprint", pluginFingerprint(key))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sanitizeKeyInput 把管理员可能粘进来的各种形式洗干净：
//
//	trusted_keys = ["XXX"]       → XXX
//	"XXX"                        → XXX
//	XXX                          → XXX
func sanitizeKeyInput(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i+1 : j]
		}
	}
	s = strings.Trim(s, " \t\r\n\"'`,{}[]")
	// 多行/逗号分隔只取第一段
	if i := strings.IndexAny(s, ",\n"); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, " \t\r\n\"'`")
}

// pluginFingerprint 直接用 plugin 包的实现，保证和 qixinpack check 显示的一致。
func pluginFingerprint(key string) string {
	return plugin.FingerprintOf(key)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}
