package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/xin/onebot/internal/config"
	"github.com/xin/onebot/internal/perm"
)

// apiSaveOwner 保存"主人 QQ"。
//
// 主人是框架级的最高权限：任何插件、任何策略都对他放行
// （perm.Checker.Check 里第一句就是 if IsOwner(userID) return true）。
// 这里改的是 config.toml 的 [owner].users，改完立即重建 checker，不用重启。
func (s *Server) apiSaveOwner(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	raw := strings.TrimSpace(r.FormValue("users"))
	// 支持 | 和逗号分隔
	fields := strings.FieldsFunc(raw, func(c rune) bool {
		return c == '|' || c == ',' || c == '，' || c == ' ' || c == '\t'
	})

	users := make([]int64, 0, len(fields))
	seen := make(map[int64]bool)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil || n <= 0 {
			http.Redirect(w, r, "/permissions?owner=bad", http.StatusSeeOther)
			return
		}
		if !seen[n] {
			seen[n] = true
			users = append(users, n)
		}
	}

	s.Deps.Config.Owner.Users = users
	if err := config.Save(s.Deps.ConfigPath, s.Deps.Config); err != nil {
		s.Log.Error("保存主人 QQ 失败", "err", err)
		http.Redirect(w, r, "/permissions?owner=savefail", http.StatusSeeOther)
		return
	}

	// 立即生效：重建权限判定器（结构体覆盖，指针不变）
	if s.Deps.Checker != nil {
		*s.Deps.Checker = *perm.New(s.Deps.Config.Permissions.Mode, users)
	}

	s.Log.Info("主人 QQ 已更新", "count", len(users))
	http.Redirect(w, r, "/permissions?owner=ok", http.StatusSeeOther)
}
