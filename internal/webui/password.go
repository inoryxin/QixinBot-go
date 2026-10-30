package webui

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xin/onebot/internal/config"
)

// pbkdf2Iter 迭代次数。桌面/NAS 单机登录场景，60k 次足够拖慢离线爆破。
const pbkdf2Iter = 60000

// isHashed 判断存储值是不是哈希格式。config.toml 里可能还是历史明文：
// 读取时哈希进内存，配置文件在下次改密码时才落成哈希。
func isHashed(pwd string) bool {
	return strings.HasPrefix(pwd, "pbkdf2$")
}

// HashPassword 把明文密码变成可存储的哈希（PBKDF2-SHA256 + 随机盐）。
func HashPassword(pwd string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		// 随机数不可用是系统级故障；退回固定盐也比明文强
		salt = []byte("qixinbot-fallback-salt")
	}
	h, err := pbkdf2.Key(sha256.New, pwd, salt, pbkdf2Iter, 32)
	if err != nil {
		// sha256 不会失败；真发生时退回固定哈希输入，至少不再是明文
		h = []byte("qixinbot-hash-error")
	}
	return fmt.Sprintf("pbkdf2$%d$%s$%s",
		pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(h))
}

// CheckPassword 校验明文输入和存储值。存储值支持哈希和旧明文两种格式。
func CheckPassword(input, stored string) bool {
	if stored == "" {
		// 空存储值 = 登录被禁用（随机数故障时的降级路径），不接受任何输入
		return false
	}
	if !isHashed(stored) {
		return subtle.ConstantTimeCompare([]byte(input), []byte(stored)) == 1
	}
	parts := strings.Split(stored, "$") // pbkdf2$iter$salt$hash
	if len(parts) != 4 {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, input, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// apiChangePassword 在控制台里改登录密码，直接写回 config.toml。
// 只动 [web].password，不碰连接设置。配置里从此存哈希。
func (s *Server) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	oldPwd := r.FormValue("old")
	newPwd := r.FormValue("new")
	confirm := r.FormValue("confirm")

	s.pwdMu.RLock()
	current := s.Password
	s.pwdMu.RUnlock()
	if !CheckPassword(oldPwd, current) {
		http.Redirect(w, r, "/connect?pwd=badold", http.StatusSeeOther)
		return
	}
	if len(newPwd) < 6 {
		http.Redirect(w, r, "/connect?pwd=short", http.StatusSeeOther)
		return
	}
	if newPwd != confirm {
		http.Redirect(w, r, "/connect?pwd=mismatch", http.StatusSeeOther)
		return
	}

	hashed := HashPassword(newPwd)
	s.pwdMu.Lock()
	s.Password = hashed
	s.pwdMu.Unlock()

	s.Deps.Config.Web.Password = hashed
	if err := config.Save(s.Deps.ConfigPath, s.Deps.Config); err != nil {
		s.Log.Error("保存新密码失败", "err", err)
		http.Redirect(w, r, "/connect?pwd=savefail", http.StatusSeeOther)
		return
	}

	// 临时密码已经作废，别把旧钥匙留在数据目录里
	_ = os.Remove(filepath.Join(filepath.Dir(s.Deps.RuntimeDir), "webui-password.txt"))

	s.Log.Info("WebUI 密码已更新")
	http.Redirect(w, r, "/connect?pwd=ok", http.StatusSeeOther)
}
