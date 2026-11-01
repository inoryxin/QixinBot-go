package webui

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xin/onebot/internal/config"
	"github.com/xin/onebot/internal/plugin"
)

// 主题系统 v2：主题包（kind="theme"）= 色板覆盖 + 自由 theme.css + assets 素材。
// 色板值在安装时已经过严格校验（纯 hex），这里只做合并与拼接；
// theme.css 是"签名即信任"的自由 CSS（和插件面板同权），安装闸门见 plugin.InstallThemeZip。

// themeDefaultLight/Dark 是"内置默认"色板（与 server.go 的 themeLightVars/DarkVars 同值）。
// 主题包只写覆盖项，缺的回落到这里——包作者不用抄全 23 个变量。
var themeDefaultLight = map[string]string{
	"bg": "#fafafa", "fg": "#222222", "panel": "#ffffff",
	"border": "#e8e8e8", "border-soft": "#f0f0f0",
	"muted": "#888888", "nav-fg": "#555555", "hover": "#f0f0f0",
	"accent": "#1a73e8", "accent-soft": "#eef3fe", "on-accent": "#ffffff",
	"input-border": "#d5d5d5", "th-bg": "#fcfcfc", "code-bg": "#f4f4f4",
	"ok": "#137333", "ok-bg": "#e6f4ea",
	"bad": "#c5221f", "bad-bg": "#fce8e6", "bad-border": "#f0c0be",
	"warn": "#e8a33d", "warn-bg": "#fff8e1", "warn-border": "#ffe08a", "warn-fg": "#7a5b00",
}

var themeDefaultDark = map[string]string{
	"bg": "#141414", "fg": "#e4e4e4", "panel": "#1e1e1e",
	"border": "#2e2e2e", "border-soft": "#262626",
	"muted": "#9a9a9a", "nav-fg": "#b5b5b5", "hover": "#2a2a2a",
	"accent": "#6ea8fe", "accent-soft": "#1c2a44", "on-accent": "#0d1b2e",
	"input-border": "#3a3a3a", "th-bg": "#242424", "code-bg": "#2a2a2a",
	"ok": "#81c995", "ok-bg": "#1e3328",
	"bad": "#f28b82", "bad-bg": "#3a2321", "bad-border": "#5c3a36",
	"warn": "#e8a33d", "warn-bg": "#382e18", "warn-border": "#5c4d1f", "warn-fg": "#ffd180",
}

// builtinThemes 是免安装的整 feel 主题（色板级）。整包级的二次元主题
// （带 CSS/素材）走主题包上传，源码在插件仓库 themes/ 下。
var builtinThemes = []struct {
	ID    string
	Name  string
	Light map[string]string
	Dark  map[string]string
}{
	{
		ID:   "sakura",
		Name: "🌸 樱花",
		Light: map[string]string{
			"bg": "#fff0f5", "fg": "#4a2c3a", "panel": "#fffdfe",
			"border": "#f2d9e3", "border-soft": "#f8e8ee",
			"muted": "#b08a9a", "nav-fg": "#7a5566", "hover": "#f9e6ee",
			"accent": "#e75480", "accent-soft": "#fde8f0", "on-accent": "#ffffff",
			"input-border": "#ecc9d6", "th-bg": "#fdf5f8", "code-bg": "#f9eef2",
			"ok": "#2e7d54", "ok-bg": "#e5f4ec",
			"bad": "#d04a6a", "bad-bg": "#fce8ee", "bad-border": "#f2c1d0",
			"warn": "#c47f17", "warn-bg": "#fff5e0", "warn-border": "#ffe1a3", "warn-fg": "#8a6100",
		},
		Dark: map[string]string{
			"bg": "#221820", "fg": "#f0dfe8", "panel": "#2c1f29",
			"border": "#3d2b37", "border-soft": "#332430",
			"muted": "#b294a4", "nav-fg": "#d5b8c6", "hover": "#3a2935",
			"accent": "#ff7fa8", "accent-soft": "#3d2130", "on-accent": "#2b1220",
			"input-border": "#4a3540", "th-bg": "#2a1e27", "code-bg": "#332430",
			"ok": "#7fd6a4", "ok-bg": "#1d3328",
			"bad": "#ff9db4", "bad-bg": "#3d2430", "bad-border": "#59323f",
			"warn": "#ffcf7a", "warn-bg": "#3a2f18", "warn-border": "#5c4d1f", "warn-fg": "#ffd9a0",
		},
	},
	{
		ID:   "yozora",
		Name: "🌌 星夜",
		Light: map[string]string{
			"bg": "#f3f6fd", "fg": "#22304f", "panel": "#ffffff",
			"border": "#d9e1f2", "border-soft": "#e8eef9",
			"muted": "#7d8dab", "nav-fg": "#55688f", "hover": "#e9eefa",
			"accent": "#4a6ee0", "accent-soft": "#e4ebfb", "on-accent": "#ffffff",
			"input-border": "#c5d1e8", "th-bg": "#f4f7fd", "code-bg": "#edf1fa",
			"ok": "#137333", "ok-bg": "#e6f4ea",
			"bad": "#c5221f", "bad-bg": "#fce8e6", "bad-border": "#f0c0be",
			"warn": "#b07d10", "warn-bg": "#fff5dd", "warn-border": "#ffe1a3", "warn-fg": "#7a5b00",
		},
		Dark: map[string]string{
			"bg": "#10141f", "fg": "#dbe4f5", "panel": "#171d2c",
			"border": "#253048", "border-soft": "#1f2739",
			"muted": "#8494b8", "nav-fg": "#b3c2e0", "hover": "#1f2940",
			"accent": "#7c9dff", "accent-soft": "#1a2440", "on-accent": "#0d1430",
			"input-border": "#2e3b58", "th-bg": "#1a2133", "code-bg": "#1f2739",
			"ok": "#66d9a3", "ok-bg": "#12291f",
			"bad": "#ff8fa3", "bad-bg": "#341d26", "bad-border": "#53303c",
			"warn": "#ffd166", "warn-bg": "#33290f", "warn-border": "#574617", "warn-fg": "#ffe0a3",
		},
	},
}

// themesDir 返回主题包安装根目录（data/themes）。
func (s *Server) themesDir() string {
	return filepath.Join(filepath.Dir(s.Deps.RuntimeDir), "themes")
}

// themePack 描述一个可选主题：内置或已安装。
type themePack struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Builtin bool   `json:"builtin"`
}

// listThemePacks 返回内置主题 + 已安装主题包（按 id 排序）。
func (s *Server) listThemePacks() []themePack {
	out := []themePack{}
	for _, b := range builtinThemes {
		out = append(out, themePack{ID: b.ID, Name: b.Name, Builtin: true})
	}
	entries, err := os.ReadDir(s.themesDir())
	if err == nil {
		var packs []themePack
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(s.themesDir(), e.Name(), "manifest.toml"))
			if err != nil {
				continue
			}
			man, err := plugin.ParseThemeManifest(raw)
			if err != nil || man.ID != e.Name() {
				continue
			}
			packs = append(packs, themePack{ID: man.ID, Name: man.Name, Version: man.Version})
		}
		sort.Slice(packs, func(i, j int) bool { return packs[i].ID < packs[j].ID })
		out = append(out, packs...)
	}
	return out
}

// themePackCSS 返回当前选中主题的覆盖 CSS（拼在内置主题 CSS 之后）。
// 内置默认返回空串。主题包：色板覆盖 + 永远最后挂 theme.css。
func (s *Server) themePackCSS() string {
	id := s.Deps.Config.Web.ThemePack
	if id == "" {
		return ""
	}
	var light, dark map[string]string
	found := false
	for _, b := range builtinThemes {
		if b.ID == id {
			light, dark = b.Light, b.Dark
			found = true
			break
		}
	}
	if !found {
		raw, err := os.ReadFile(filepath.Join(s.themesDir(), id, "manifest.toml"))
		if err != nil {
			return "" // 主题包没了：静默回落内置
		}
		man, err := plugin.ParseThemeManifest(raw)
		if err != nil {
			return ""
		}
		light = mergePalette(themeDefaultLight, man.Light)
		dark = mergePalette(themeDefaultDark, man.Dark)
		found = true
	}
	if !found {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n/* 主题包覆盖 */\n:root{")
	writeVars(&b, light)
	b.WriteString("}\n:root[data-theme=dark]{")
	writeVars(&b, dark)
	b.WriteString("}\n@media (prefers-color-scheme: dark){:root[data-theme=auto]{")
	writeVars(&b, dark)
	b.WriteString("}}\n")
	// 主题包的自由 CSS 永远最后加载：签名即信任，能整体改观感
	if css, err := os.ReadFile(filepath.Join(s.themesDir(), id, "theme.css")); err == nil && len(css) > 0 {
		b.WriteString("\n/* theme.css（主题包自带） */\n")
		b.Write(css)
	}
	return b.String()
}

func mergePalette(base, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func writeVars(b *strings.Builder, palette map[string]string) {
	keys := make([]string, 0, len(palette))
	for k := range palette {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("--" + k + ":" + palette[k] + ";")
	}
}

// themePackHasCSS 报告当前主题包是否带 theme.css（render 时决定挂不挂）。
func (s *Server) themePackHasCSS() bool {
	id := s.Deps.Config.Web.ThemePack
	if id == "" {
		return false
	}
	for _, b := range builtinThemes {
		if b.ID == id {
			return false
		}
	}
	st, err := os.Stat(filepath.Join(s.themesDir(), id, "theme.css"))
	return err == nil && !st.IsDir() && st.Size() > 0
}

// ---- 接口 ----

// apiThemeSelect 切换主题（写入 config 并落盘）。空 id = 回到内置默认。
func (s *Server) apiThemeSelect(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("pack"))
	if id != "" && !validPluginPathID(id) {
		http.Error(w, "非法主题 id", http.StatusBadRequest)
		return
	}
	if id != "" {
		found := false
		for _, p := range s.listThemePacks() {
			if p.ID == id {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "主题不存在: "+id, http.StatusBadRequest)
			return
		}
	}
	s.Deps.Config.Web.ThemePack = id
	if err := config.Save(s.Deps.ConfigPath, s.Deps.Config); err != nil {
		http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Log.Info("界面主题已切换", "theme", id)
	http.Redirect(w, r, "/overview?theme=ok", http.StatusSeeOther)
}

// apiThemeUpload 上传主题包（.anzu，kind="theme"）。签名闸门在 plugin 包里。
func (s *Server) apiThemeUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "读取上传失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "没有选择文件", http.StatusBadRequest)
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 8<<20+1))
	if err != nil {
		http.Error(w, "读取失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(raw) > 8<<20 {
		http.Error(w, "主题包超过 8MB 上限", http.StatusBadRequest)
		return
	}
	if s.Deps.Mgr == nil || s.Deps.Mgr.Inst == nil {
		http.Error(w, "安装器未就绪", http.StatusInternalServerError)
		return
	}
	id, err := s.Deps.Mgr.Inst.InstallThemeZip(raw, s.themesDir())
	if err != nil {
		s.Log.Warn("主题包安装失败", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.Log.Info("主题包已上传", "theme", id)
	http.Redirect(w, r, "/overview?theme=uploaded", http.StatusSeeOther)
}

// apiThemeRemove 删除已安装的主题包；正在使用中的拒绝先切换再删。
func (s *Server) apiThemeRemove(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if !validPluginPathID(id) {
		http.Error(w, "非法主题 id", http.StatusBadRequest)
		return
	}
	if s.Deps.Config.Web.ThemePack == id {
		http.Error(w, "该主题正在使用，先切换到其它主题再删除", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.themesDir(), id)
	// 双重保险：目录必须真的在 themesDir 里
	if !strings.HasPrefix(filepath.Clean(dir)+string(os.PathSeparator), filepath.Clean(s.themesDir())+string(os.PathSeparator)) {
		http.Error(w, "非法路径", http.StatusBadRequest)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		http.Error(w, "删除失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Log.Info("主题包已删除", "theme", id)
	http.Redirect(w, r, "/overview?theme=removed", http.StatusSeeOther)
}

var _ = errors.Is // 保留：后续错误分类用

// themeCSSLink 当前主题带 theme.css 时返回 <link>；没有就空串。
func (s *Server) themeCSSLink() string {
	if !s.themePackHasCSS() {
		return ""
	}
	id := s.Deps.Config.Web.ThemePack
	// mtime 当版本号：主题包更新后浏览器缓存自动失效
	st, err := os.Stat(filepath.Join(s.themesDir(), id, "theme.css"))
	v := "0"
	if err == nil {
		v = fmt.Sprintf("%d", st.ModTime().Unix())
	}
	return `<link rel="stylesheet" href="/ui/theme/` + id + `/theme.css?v=` + v + `">`
}

// handleThemeAssets 提供主题包的 theme.css 和 assets/（背景图等）。
// id 过 ID 白名单，文件名过 http.FileServer（根目录锁死在主题自己的 assets/theme.css 范围）。
func (s *Server) handleThemeAssets(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ui/theme/")
	id, tail, _ := strings.Cut(rest, "/")
	if !validPluginPathID(id) {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(s.themesDir(), id)
	if tail == "theme.css" {
		p := filepath.Join(dir, "theme.css")
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		http.ServeFile(w, r, p)
		return
	}
	if tail == "" || tail == "/" {
		http.NotFound(w, r)
		return
	}
	// 只允许 assets/ 下的东西
	if tail != "assets" && !strings.HasPrefix(tail, "assets/") {
		http.NotFound(w, r)
		return
	}
	assetsDir := filepath.Join(dir, "assets")
	if !dirExistsLocal(assetsDir) {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/ui/theme/"+id+"/assets/", http.FileServer(http.Dir(assetsDir))).ServeHTTP(w, r)
}

func dirExistsLocal(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
