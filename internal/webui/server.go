package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xin/onebot/internal/audit"
	"github.com/xin/onebot/internal/chatbuf"
	"github.com/xin/onebot/internal/config"
	"github.com/xin/onebot/internal/logbuf"
	"github.com/xin/onebot/internal/metrics"
	"github.com/xin/onebot/internal/monitor"
	"github.com/xin/onebot/internal/onebot"
	"github.com/xin/onebot/internal/override"
	"github.com/xin/onebot/internal/perm"
	"github.com/xin/onebot/internal/plugin"
	"github.com/xin/onebot/internal/registry"
	"github.com/xin/onebot/internal/send"
	"github.com/xin/onebot/internal/session"
)

const (
	cookieName     = "qb_session"
	csrfCookieName = "qixin_csrf"
	sessionTTL     = 7 * 24 * time.Hour
)

// Deps 是控制台需要的运行时依赖。
type Deps struct {
	StartedAt  time.Time
	Version    string
	Config     *config.Config
	ConfigPath string
	Clients    map[string]*onebot.Client
	RuntimeDir string
	Mgr        *plugin.Manager
	Checker    *perm.Checker
	Reg        *registry.Registry
	Overrides  *override.Store
	Logs       *logbuf.Buffer
	Audit      *audit.Logger
	Chat       *chatbuf.Buffer
	Monitor    *monitor.Sampler
	Queue      *send.Queue
	Metrics    *metrics.Collector

	// Inst 用于插件安装/签名校验（信任列表热更新要改它的 keys）
	Inst *plugin.Installer
	// Trust 是运行期添加的可信公钥（落盘 data/trusted-keys.json）
	Trust *plugin.TrustStore

	// Builtin 是框架自带命令（/help、/plugins）的处理入口。
	//
	// 控制台注入必须也走它——否则测试和真实链路不一致：
	// 真实消息会先被内置命令吃掉，注入却直接进插件，
	// 于是"插件抢了内置命令"这种冲突在测试里看不出来。
	Builtin func(ctx context.Context, s *session.Session) bool
}

// Server 是控制台。
type Server struct {
	Addr     string
	Username string
	// TrustedProxies 是可信代理地址；不在列表里的来源，X-Forwarded-For 一律忽略
	TrustedProxies []string
	Password       string
	Deps           *Deps
	Log            *slog.Logger

	srv *http.Server

	sessMu   sync.Mutex
	sessions map[string]time.Time
	cfgMu    sync.Mutex
	sessFile string

	// 登录失败限速：同一 IP 连续失败 5 次锁 5 分钟
	failMu    sync.Mutex
	failCount map[string]*failEntry
	pwdMu     sync.RWMutex
}

// New 创建控制台。
func New(addr, username, password string, deps *Deps, log *slog.Logger) *Server {
	logger := log.With("component", "webui")
	if username == "" {
		username = "admin"
	}
	if password == "" {
		tok, err := randomToken()
		if err != nil {
			// 随机数不可用：不能降级成已知令牌，直接禁用登录
			logger.Error("系统随机数不可用，WebUI 登录已禁用", "err", err)
			password = ""
		} else {
			password = tok
			// 临时密码只写数据目录的专用文件（0600），绝不进日志：
			// 日志文件是 0644，还会被 /api/logs 和备份带走，等于把钥匙贴门上
			pwdFile := filepath.Join(filepath.Dir(deps.RuntimeDir), "webui-password.txt")
			if werr := os.WriteFile(pwdFile, []byte(tok+"\n"), 0o600); werr != nil {
				// 文件写不进去就只能打印一次，保底别让管理员进不来
				logger.Warn("未配置 WebUI 密码且临时密码文件写入失败，仅在本次输出", "err", werr, "password", tok)
			} else {
				logger.Warn("未配置 WebUI 密码，本次启动使用临时密码，见数据目录 webui-password.txt（登录后请尽快修改）")
			}
		}
	} else {
		// 换过密码后配置里已经是哈希；历史明文在这里就地哈希，
		// 内存里只留哈希，配置文件等下次改密码时自然落成哈希
		if !isHashed(password) {
			password = HashPassword(password)
		}
		// 临时密码文件是上次自动生成的，这次用的是配置密码，清掉残留
		_ = os.Remove(filepath.Join(filepath.Dir(deps.RuntimeDir), "webui-password.txt"))
	}
	srv := &Server{
		Addr:     addr,
		Username: username,
		Password: password,
		Deps:     deps,
		Log:      logger,
		sessions: make(map[string]time.Time),
		sessFile: filepath.Join(filepath.Dir(deps.RuntimeDir), "sessions.json"),
	}
	srv.loadSessions()
	return srv
}

// Listen 启动服务（阻塞）。
func (s *Server) Listen() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	// 主题包：切换/上传/删除 + 素材与 theme.css 的静态供给
	mux.HandleFunc("/api/theme/select", s.guard(s.apiThemeSelect))
	mux.HandleFunc("/api/theme/upload", s.guard(s.apiThemeUpload))
	mux.HandleFunc("/api/theme/remove", s.guard(s.apiThemeRemove))
	mux.HandleFunc("/ui/theme/", s.guard(s.handleThemeAssets))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", s.handleReady)

	mux.HandleFunc("/", s.guard(s.handleRoot))
	for _, p := range []string{"/overview", "/connect", "/permissions", "/plugins", "/cohost", "/help-settings", "/logs", "/tools", "/audit", "/chat"} {
		path := p
		mux.HandleFunc(path, s.guard(func(w http.ResponseWriter, r *http.Request) { s.handlePage(w, r, path) }))
	}

	mux.HandleFunc("POST /api/connect", s.guard(s.apiSaveConnect))
	mux.HandleFunc("POST /api/password", s.guard(s.apiChangePassword))
	mux.HandleFunc("POST /api/permissions", s.guard(s.apiSavePermissions))
	mux.HandleFunc("POST /api/priority", s.guard(s.apiSavePriority))
	mux.HandleFunc("POST /api/owner", s.guard(s.apiSaveOwner))
	mux.HandleFunc("POST /api/help", s.guard(s.apiSaveHelp))
	mux.HandleFunc("GET /api/trusted-keys", s.guard(s.apiTrustedKeys))
	mux.HandleFunc("POST /api/trusted-keys", s.guard(s.apiAddTrustedKey))
	mux.HandleFunc("POST /api/trusted-keys/delete", s.guard(s.apiRemoveTrustedKey))
	mux.HandleFunc("POST /api/plugins/upload", s.guard(s.apiUploadPlugin))
	mux.HandleFunc("POST /api/plugins/{id}/{action}", s.guard(s.apiPluginAction))
	mux.HandleFunc("GET /api/cohost/pools", s.guard(s.apiCohostPools))
	mux.HandleFunc("POST /api/cohost/pools", s.guard(s.apiCohostCreatePool))
	mux.HandleFunc("POST /api/cohost/pools/update", s.guard(s.apiCohostUpdatePool))
	mux.HandleFunc("POST /api/cohost/pools/delete", s.guard(s.apiCohostDeletePool))
	mux.HandleFunc("POST /api/cohost/assign", s.guard(s.apiCohostAssign))
	mux.HandleFunc("/api/logs", s.guard(s.apiLogs))
	mux.HandleFunc("/api/tools", s.guard(s.apiTools))
	mux.HandleFunc("POST /api/tools/call", s.guard(s.apiCallTool))
	mux.HandleFunc("POST /api/capabilities/call", s.guard(s.apiCallCapability))
	mux.HandleFunc("GET /api/capabilities", s.guard(s.apiCapabilities))
	mux.HandleFunc("GET /api/metrics", s.guard(s.apiMetrics))
	mux.HandleFunc("POST /api/chat/inject", s.guard(s.apiInjectMessage))
	mux.HandleFunc("GET /api/chat/messages", s.guard(s.apiChatMessages))
	mux.HandleFunc("POST /api/chat/send", s.guard(s.apiChatSend))
	mux.HandleFunc("GET /api/chat/contacts", s.guard(s.apiChatContacts))
	mux.HandleFunc("GET /api/chat/recent", s.guard(s.apiChatRecent))
	mux.HandleFunc("GET /api/chat/image", s.guard(s.apiChatImage))
	mux.HandleFunc("/ui/qixin-form.js", s.handleFormJS)
	mux.HandleFunc("/plugin/", s.guard(s.handlePluginWeb))
	mux.HandleFunc("/api/plugin/{id}/settings", s.guard(s.handlePluginSettings))
	mux.HandleFunc("POST /api/plugin/{id}/settings/restore", s.guard(s.handlePluginSettingsRestore))
	mux.HandleFunc("/api/plugin/{id}/http/", s.guard(s.handlePluginHTTP))
	mux.HandleFunc("POST /api/runtime", s.guard(s.apiRuntimePaths))

	mux.HandleFunc("/deps", s.guard(s.pageDeps))
	mux.HandleFunc("POST /api/deps/install", s.guard(s.apiInstallDep))
	mux.HandleFunc("/api/overview", s.guard(s.apiOverview))
	// 卡死时把全部 goroutine 堆栈打出来，用于定位死锁
	mux.HandleFunc("/debug/stack", s.guard(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(buf[:n])
	}))

	s.srv = &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	s.Log.Info("控制台已启动", "url", "http://"+s.Addr, "user", s.Username)
	return s.srv.ListenAndServe()
}

// Close 关闭服务。
func (s *Server) Close(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(c)
}

func isUnsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// sameOrigin 校验写操作的 Origin；没有 Origin（curl 等）放行，但 POST 仍需 CSRF token。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// csrfOK 用会话令牌做双提交校验：表单 _csrf 或请求头 X-CSRF-Token。
func (s *Server) csrfOK(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	tok := r.Header.Get("X-CSRF-Token")
	if tok == "" {
		tok = r.FormValue("_csrf")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(c.Value)) == 1
}

func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isUnsafeMethod(r.Method) {
			if !sameOrigin(r) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
			if s.authed(r) && !s.csrfOK(r) {
				http.Error(w, "csrf token missing or invalid", http.StatusForbidden)
				return
			}
		}
		if s.authed(r) {
			// 浏览器只剩会话 cookie、CSRF cookie 被清掉时，补一次；
			// 否则面板里所有 POST 都会 403，前端只看到 JSON.parse 失败。
			if c, err := r.Cookie(csrfCookieName); err != nil || c.Value == "" {
				if sess, err2 := r.Cookie(cookieName); err2 == nil && sess.Value != "" {
					http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: sess.Value, Path: "/", HttpOnly: false})
				}
			}
			next(w, r)
			return
		}
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// loadSessions 从磁盘读登录态：重启机器人不用重新登录。
func (s *Server) loadSessions() {
	raw, err := os.ReadFile(s.sessFile)
	if err != nil {
		return
	}
	var m map[string]time.Time
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	now := time.Now()
	for k, exp := range m {
		if exp.After(now) {
			s.sessions[k] = exp
		}
	}
	s.Log.Info("已恢复登录态", "sessions", len(s.sessions))
}

// saveSessions 把登录态写盘（登录/登出时调用）。
func (s *Server) saveSessions() {
	s.sessMu.Lock()
	out := make(map[string]time.Time, len(s.sessions))
	now := time.Now()
	for k, exp := range s.sessions {
		if exp.After(now) {
			out[k] = exp
		}
	}
	s.sessMu.Unlock()

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.sessFile, raw, 0o600); err != nil {
		s.Log.Warn("保存登录态失败", "err", err)
	}
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	s.sessMu.Lock()
	exp, ok := s.sessions[c.Value]
	expired := ok && time.Now().After(exp)
	if expired {
		delete(s.sessions, c.Value)
	}
	s.sessMu.Unlock()

	if expired {
		go s.saveSessions()
		return false
	}
	return ok
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.Password == "" {
		http.Error(w, "WebUI 登录已禁用（系统随机数不可用），请配置固定密码", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, s.renderLogin(""))
		return
	}

	_ = r.ParseForm()
	user := r.FormValue("username")
	pass := r.FormValue("password")

	// 先看这个 IP 是不是被锁了
	ip := s.clientIP(r)
	if wait, locked := s.loginLocked(ip); locked {
		s.Log.Warn("登录被限速", "ip", ip, "wait", wait)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, s.renderLogin(fmt.Sprintf("尝试次数过多，请 %d 秒后再试", int(wait.Seconds()))))
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.Username)) == 1
	s.pwdMu.RLock()
	current := s.Password
	s.pwdMu.RUnlock()
	passOK := CheckPassword(pass, current)
	if !userOK || !passOK {
		s.recordLoginFailure(ip)
		s.Log.Warn("登录失败", "user", user, "remote", ip)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, s.renderLogin("账号或密码错误"))
		return
	}
	s.clearLoginFailures(ip)

	token, err := randomToken()
	if err != nil {
		http.Error(w, "系统随机数不可用，无法登录", http.StatusInternalServerError)
		return
	}
	s.sessMu.Lock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.sessMu.Unlock()
	s.saveSessions()

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
		// 走 HTTPS（包括反代）时加 Secure，防止明文通道泄 Cookie
		Secure: isSecureRequest(r),
	})
	s.Log.Info("登录成功", "user", user, "remote", r.RemoteAddr)
	http.Redirect(w, r, "/overview", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// 只收 POST：页面上是表单按钮。GET 退出会被任何页面里的
	// <a href=".../logout"> 利用（Lax 不拦顶层 GET 导航），把管理员登出去
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/overview", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
		s.saveSessions() // 关键：退出要落盘，否则重启后旧会话又活了
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/overview", http.StatusSeeOther)
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---- 布局 ----

func navItem(href, label, active string) string {
	cls := ""
	if href == active {
		cls = ` class="active"`
	}
	return fmt.Sprintf(`<a href="%s"%s>%s</a>`, href, cls, label)
}

func (s *Server) render(w http.ResponseWriter, title, active, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>%s · QixinBot</title><style>%s%s</style>%s

<script>
// 主题必须在首帧之前就位，否则深色用户每次刷新都白闪一下
(function(){
  try{ document.documentElement.dataset.theme = localStorage.getItem('qb-theme') || 'auto'; }catch(e){}
})();
(function(){
  var m=document.cookie.match(/(?:^|; )qixin_csrf=([^;]+)/);
  var t=m?decodeURIComponent(m[1]):'';
  if(!t) return;
  var of=window.fetch;
  window.fetch=function(input,init){
    init=init||{};
    var h=new Headers(init.headers||{});
    if(!h.has('X-CSRF-Token')) h.set('X-CSRF-Token',t);
    init.headers=h;
    return of(input,init);
  };
  document.addEventListener('DOMContentLoaded',function(){
    document.querySelectorAll('form').forEach(function(f){
      if(f.querySelector('input[name=_csrf]')) return;
      var i=document.createElement('input'); i.type='hidden'; i.name='_csrf'; i.value=t; f.appendChild(i);
    });
    var sel=document.getElementById('theme-select');
    if(sel){
      sel.value=document.documentElement.dataset.theme||'auto';
      sel.addEventListener('change',function(){
        try{ localStorage.setItem('qb-theme',sel.value); }catch(e){}
        document.documentElement.dataset.theme=sel.value;
      });
    }
  });
  // 删除按钮：用 fetch 提交，失败时把服务端错误直接弹出来，
  // 避免只看到页面刷新、却不知道删除到底成没成功。
  document.addEventListener('submit',function(e){
    var f=e.target;
    if(!f || !f.classList || !f.classList.contains('js-delete')) return;
    e.preventDefault();
    var btn=f.querySelector('button');
    // 不用 confirm()：浏览器勾了"阻止此页面创建更多对话框"后会静默返回 false，
    // 表现就是"点删除没反应"。改成两次点击确认。
    if(!f.dataset.armed){
      f.dataset.armed='1';
      if(btn){ btn.dataset.text=btn.textContent; btn.textContent='再点确认'; }
      setTimeout(function(){ f.dataset.armed=''; if(btn && btn.dataset.text) btn.textContent=btn.dataset.text; },3000);
      return;
    }
    fetch(f.action,{method:'POST',headers:{'X-CSRF-Token':t}}).then(function(r){
      if(r.redirected){location.href=r.url; return;}
      return r.text().then(function(x){ alert('删除失败：'+x); });
    }).catch(function(err){ alert('删除失败：'+err.message); });
  },true);
})();
</script>
</head><body>
<div class="nav">
  <div class="brand">QixinBot</div>
  <div class="links">
    %s
  </div>
  <div class="right">
    <select id="theme-select" title="界面主题" style="display:inline;width:auto;padding:2px 6px;font-size:12px;margin-right:8px">
      <option value="auto">跟随系统</option>
      <option value="light">浅色</option>
      <option value="dark">深色</option>
    </select>
    <form method="post" action="/logout" style="display:inline"><button style="background:none;border:none;color:inherit;cursor:pointer;padding:0">退出</button></form>
  </div>
</div>
<div class="wrap">%s</div>

</body></html>`,
		esc(title), appCSS, s.themePackCSS(), s.themeCSSLink(),
		navItem("/overview", "概览", active)+
			navItem("/connect", "连接", active)+
			navItem("/permissions", "权限", active)+
			navItem("/plugins", "插件", active)+
			navItem("/cohost", "共享池", active)+
			navItem("/deps", "依赖", active)+
			navItem("/help-settings", "指令", active)+
			navItem("/chat", "聊天", active)+
			navItem("/audit", "审计", active)+
			navItem("/logs", "日志", active),
		body)
}

// themeVars 是浅色主题的调色板（默认）。所有页面颜色都必须走这些变量，
// 禁止新写硬编码色值——深色主题靠整组变量翻转，散落一个硬编码就是一块补丁。
const themeLightVars = `
:root{
  color-scheme:light;
  --bg:#fafafa; --fg:#222; --panel:#fff;
  --border:#e8e8e8; --border-soft:#f0f0f0;
  --muted:#888; --nav-fg:#555; --hover:#f0f0f0;
  --accent:#1a73e8; --accent-soft:#eef3fe; --on-accent:#fff;
  --input-border:#d5d5d5; --th-bg:#fcfcfc; --code-bg:#f4f4f4;
  --ok:#137333; --ok-bg:#e6f4ea; --bad:#c5221f; --bad-bg:#fce8e6; --bad-border:#f0c0be;
  --warn:#e8a33d; --warn-bg:#fff8e1; --warn-border:#ffe08a; --warn-fg:#7a5b00;
}`

// themeDarkVars 是深色主题的变量覆盖。
const themeDarkVars = `
  color-scheme:dark;
  --bg:#141414; --fg:#e4e4e4; --panel:#1e1e1e;
  --border:#2e2e2e; --border-soft:#262626;
  --muted:#9a9a9a; --nav-fg:#b5b5b5; --hover:#2a2a2a;
  --accent:#6ea8fe; --accent-soft:#1c2a44; --on-accent:#0d1b2e;
  --input-border:#3a3a3a; --th-bg:#242424; --code-bg:#2a2a2a;
  --ok:#81c995; --ok-bg:#1e3328; --bad:#f28b82; --bad-bg:#3a2321; --bad-border:#5c3a36;
  --warn:#e8a33d; --warn-bg:#382e18; --warn-border:#5c4d1f; --warn-fg:#ffd180;
`

// 组装主题 CSS：浅色为默认；深色在 html[data-theme=dark] 生效；
// data-theme=auto 跟随系统（prefers-color-scheme）。
const themeCSS = themeLightVars + `
:root[data-theme=dark]{` + themeDarkVars + `}
@media (prefers-color-scheme: dark){
  :root[data-theme=auto]{` + themeDarkVars + `}
}`

const appCSS = themeCSS + `
.chat-wrap{display:flex;gap:14px;min-height:520px}
.chat-side{width:200px;border:1px solid var(--border);border-radius:10px;overflow:auto;background:var(--panel)}
.chat-item{padding:10px 12px;border-bottom:1px solid var(--border);cursor:pointer;font-size:13px;color:var(--fg)}
.chat-item:hover{background:var(--hover)}
.chat-item.active{background:var(--hover);font-weight:600}
.chat-main{flex:1;display:flex;flex-direction:column;border:1px solid var(--border);border-radius:10px;background:var(--panel)}
.chat-msgs{flex:1;overflow:auto;padding:14px;max-height:460px}
.chat-row{margin-bottom:12px}
.chat-row.out{text-align:right}
.chat-meta{font-size:11px;color:var(--muted);margin-bottom:3px}
.chat-bubble{display:inline-block;padding:7px 11px;border-radius:10px;background:var(--hover);max-width:75%;text-align:left;white-space:pre-wrap;word-break:break-word;color:var(--fg)}
.chat-row.out .chat-bubble{background:var(--accent);color:var(--on-accent)}
.chat-form{display:flex;gap:8px;padding:10px;border-top:1px solid var(--border)}
.chat-form input[type=number]{width:110px}
.chat-form input[type=text]{flex:1}

*{box-sizing:border-box}
body{margin:0;font:14px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif;color:var(--fg);background:var(--bg)}
.nav{display:flex;align-items:center;gap:24px;padding:0 24px;height:52px;background:var(--panel);border-bottom:1px solid var(--border)}
.brand{font-weight:600;font-size:15px}
.links{display:flex;gap:4px;flex:1}
.links a{padding:6px 12px;border-radius:6px;color:var(--nav-fg);text-decoration:none;font-size:13px}
.links a:hover{background:var(--hover)}
.links a.active{background:var(--accent-soft);color:var(--accent);font-weight:500}
.right a{color:var(--muted);font-size:13px;text-decoration:none}
#theme-select{width:auto;padding:2px 6px;font-size:12px;background:var(--panel);color:var(--fg);border-color:var(--input-border)}
.wrap{max-width:1080px;margin:28px auto;padding:0 24px}
h1{font-size:19px;margin:0 0 4px}
h2{font-size:14px;margin:28px 0 8px;color:var(--fg)}
p.sub{color:var(--muted);font-size:13px;margin:0 0 20px}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:12px;margin:16px 0}
.card{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:14px 16px}
.card .k{color:var(--muted);font-size:12px}
.card .v{font-size:20px;font-weight:600;margin-top:2px}
.card .v.small{font-size:14px;font-weight:500}
table{border-collapse:collapse;width:100%;background:var(--panel);border:1px solid var(--border);border-radius:8px;overflow:hidden;color:var(--fg)}
th,td{text-align:left;padding:8px 12px;border-bottom:1px solid var(--border-soft);vertical-align:middle}
th{color:var(--muted);font-weight:500;font-size:12px;background:var(--th-bg)}
tr:last-child td{border-bottom:none}
code{background:var(--code-bg);padding:1px 5px;border-radius:3px;font-size:12px}
button{font-size:12px;padding:4px 12px;border:1px solid var(--input-border);background:var(--panel);color:var(--fg);border-radius:4px;cursor:pointer}
button:hover{background:var(--hover)}
button.primary{background:var(--accent);color:var(--on-accent);border-color:var(--accent)}
button.danger{color:var(--bad);border-color:var(--bad-border)}
input[type=text],input[type=password],input[type=number],select{width:100%;padding:6px 9px;border:1px solid var(--input-border);border-radius:4px;font-size:13px;background:var(--panel);color:var(--fg)}
label{font-size:12px;color:var(--muted);display:block;margin:12px 0 4px}
.ok{color:var(--ok)}.bad{color:var(--bad)}
.muted{color:var(--muted);font-size:12px}
.err{color:var(--bad);font-size:12px;max-width:260px;word-break:break-all}
.row{display:flex;gap:8px;align-items:center}
.form-box{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:20px;max-width:560px}
.actions{display:flex;gap:6px;justify-content:flex-end}
.notice{background:var(--warn-bg);border:1px solid var(--warn-border);color:var(--warn-fg);padding:10px 14px;border-radius:6px;margin:16px 0;font-size:13px}
.tag{display:inline-block;padding:1px 7px;border-radius:10px;font-size:11px;background:var(--hover);color:var(--nav-fg)}
/* 指令显示页：每个插件是一组可折叠的 */
details.hg{border:1px solid var(--border);border-radius:6px;background:var(--panel);margin-bottom:4px;overflow:hidden}
details.hg>summary{display:flex;align-items:center;gap:8px;padding:5px 10px;cursor:pointer;user-select:none;list-style:none;font-size:13px;line-height:1.4;color:var(--fg)}
details.hg>summary::-webkit-details-marker{display:none}
details.hg>summary:hover{background:var(--hover)}
details.hg[open]>summary{border-bottom:1px solid var(--border-soft);background:var(--th-bg)}
details.hg>summary .arrow{width:9px;color:var(--muted);font-size:9px;transition:transform .15s}
details.hg[open]>summary .arrow{transform:rotate(90deg)}
details.hg>summary code.id{font-size:11px;color:var(--muted);background:none;padding:0}
details.hg>summary .tag{padding:0 6px;font-size:10px;line-height:16px}
details.hg .body{padding:0}
details.hg table{border:none;border-radius:0}
details.hg table th{display:none}
details.hg table td{padding:3px 8px;border-bottom:1px solid var(--border-soft);font-size:12px}
details.hg table tr:last-child td{border-bottom:none}
.tag.ok{background:var(--ok-bg);color:var(--ok)}
.tag.bad{background:var(--bad-bg);color:var(--bad)}
.logbox{background:#1e1e1e;color:#ddd;font:12px/1.5 ui-monospace,Consolas,monospace;padding:12px;border-radius:8px;height:60vh;overflow:auto}
.logbox .lv-ERROR{color:#ff8a80}
.logbox .lv-WARN{color:#ffd180}
.logbox .lv-INFO{color:#b9f6ca}
`

const loginCSS = appCSS + `
body.login{display:flex;align-items:center;justify-content:center;height:100vh;background:var(--bg)}
.login-box{background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:32px;width:340px}
.login-box h1{margin:0 0 4px}
.login-box button{width:100%;padding:9px;font-size:14px;margin-top:6px}
`

// renderLogin 服务端入口：把当前主题包的 CSS 带进登录页。
func (s *Server) renderLogin(msg string) string {
	return loginPageWithMessage(msg, s.themePackCSS()+s.themeCSSLink())
}

// loginPageWithMessage 渲染登录页，msg 为空表示不显示提示。
// extraHead 是主题包注入的 <style>/<link>（登录页也吃主题）。
func loginPageWithMessage(text, extraHead string) string {
	msg := ""
	if text != "" {
		msg = `<div class="err">` + html.EscapeString(text) + `</div>`
	}
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>登录 · QixinBot</title><style>` + loginCSS + extraHead + `</style>
<script>(function(){try{document.documentElement.dataset.theme=localStorage.getItem('qb-theme')||'auto';}catch(e){}})();</script></head>
<body class="login"><div class="login-box">
<h1>QixinBot</h1>
<p class="muted" style="margin-bottom:16px">登录后管理插件与权限</p>
` + msg + `
<form method="post" action="/login">
  <label>账号</label><input name="username" autocomplete="username" autofocus>
  <label>密码</label><input name="password" type="password" autocomplete="current-password">
  <button class="primary" type="submit">登录</button>
</form>
</div></body></html>`
}

func esc(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '<':
			out = append(out, []rune("&lt;")...)
		case '>':
			out = append(out, []rune("&gt;")...)
		case '&':
			out = append(out, []rune("&amp;")...)
		case '"':
			out = append(out, []rune("&quot;")...)
		case '\'':
			out = append(out, []rune("&#39;")...)
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// failEntry 记录某个 IP 的登录失败情况。
type failEntry struct {
	last     time.Time
	count    int
	lockedAt time.Time
}

const (
	loginMaxFailures = 5
	loginLockWindow  = 5 * time.Minute
)

// clientIP 取客户端 IP。
//
// 安全要点：默认**只信 TCP 层的 RemoteAddr**。
// X-Forwarded-For 是客户端可以随便写的头——如果无条件相信，
// 攻击者每次换个假 IP 就能绕过登录限速。
// 只有请求确实来自配置的可信代理时，才采用转发头。
func (s *Server) clientIP(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}

	if s.isTrustedProxy(remote) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// **从右往左**取：代理通常把真实客户端追加到右边。
			// 如果直接取第一项，攻击者只要先发一个 "X-Forwarded-For: 1.2.3.4"，
			// 代理再追加真实 IP，第一项就是伪造的。
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				ip := strings.TrimSpace(parts[i])
				if ip == "" {
					continue
				}
				// 跳过我们自己的代理地址，取第一个非代理的
				if s.isTrustedProxy(ip) {
					continue
				}
				return ip
			}
			// 全是可信代理（单层代理场景），取最后一项
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return remote
}

// isTrustedProxy 判断来源地址是不是配置里允许的代理。
func (s *Server) isTrustedProxy(addr string) bool {
	if len(s.TrustedProxies) == 0 {
		return false
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, raw := range s.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// 支持 CIDR 和单个 IP 两种写法
		if strings.Contains(raw, "/") {
			if _, cidr, err := net.ParseCIDR(raw); err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		if other := net.ParseIP(raw); other != nil && other.Equal(ip) {
			return true
		}
	}
	return false
}

// isSecureRequest 判断当前请求是不是走 HTTPS（含反代终止 TLS 的情况）。
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// loginLocked 检查这个 IP 是否还在锁定期，返回剩余时长。
func (s *Server) loginLocked(ip string) (time.Duration, bool) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	e := s.failCount[ip]
	if e == nil || e.lockedAt.IsZero() {
		return 0, false
	}
	left := loginLockWindow - time.Since(e.lockedAt)
	if left <= 0 {
		// 锁过期，清零重来
		delete(s.failCount, ip)
		return 0, false
	}
	return left, true
}

// recordLoginFailure 记一次失败，到阈值就上锁。
func (s *Server) recordLoginFailure(ip string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.failCount == nil {
		s.failCount = make(map[string]*failEntry)
	}
	s.pruneLoginFailuresLocked()
	e := s.failCount[ip]
	if e == nil {
		e = &failEntry{}
		s.failCount[ip] = e
	}
	e.last = time.Now()
	e.count++
	if e.count >= loginMaxFailures {
		e.lockedAt = time.Now()
	}
}

// pruneLoginFailuresLocked 清掉过期失败记录，并给 map 设上限，
// 防止大量来源 IP 的失败请求把内存撑大。调用方需持有 failMu。
func (s *Server) pruneLoginFailuresLocked() {
	now := time.Now()
	for ip, e := range s.failCount {
		if e == nil {
			delete(s.failCount, ip)
			continue
		}
		if e.lockedAt.IsZero() && !e.last.IsZero() && now.Sub(e.last) > time.Hour {
			delete(s.failCount, ip)
		}
	}
	const maxEntries = 4096
	for len(s.failCount) > maxEntries {
		for ip := range s.failCount {
			delete(s.failCount, ip)
			break
		}
	}
}

// clearLoginFailures 登录成功后清掉计数。
func (s *Server) clearLoginFailures(ip string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	delete(s.failCount, ip)
}
