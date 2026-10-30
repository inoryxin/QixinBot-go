package plugin

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 主题包（kind="theme"）是纯外观资源：
//   manifest.toml            必需，kind="theme" + 可选 [theme.light]/[theme.dark] 色板覆盖
//   theme.css                可选，自由 CSS，控制台页面最后加载（能整体改观感）
//   assets/                  可选，背景图等素材，控制台经 /ui/theme/<id>/assets/ 提供
//
// 信任模型和插件一致：包要过签名闸门；theme.css 是"签名即信任"的产物
// （和插件面板 panel.html 同级），所以上传入口必须走 InstallThemeZip 的闸门，
// 不要绕开它直接解包。

// themePaletteKeys 是色板允许的键（TOML snake_case → CSS 变量名后缀）。
// 不在表里的键直接报错：主题包是数据，多一个未知字段都值得人工看一眼。
var themePaletteKeys = map[string]string{
	"bg": "bg", "fg": "fg", "panel": "panel",
	"border": "border", "border_soft": "border-soft",
	"muted": "muted", "nav_fg": "nav-fg", "hover": "hover",
	"accent": "accent", "accent_soft": "accent-soft", "on_accent": "on-accent",
	"input_border": "input-border", "th_bg": "th-bg", "code_bg": "code-bg",
	"ok": "ok", "ok_bg": "ok-bg",
	"bad": "bad", "bad_bg": "bad-bg", "bad_border": "bad-border",
	"warn": "warn", "warn_bg": "warn-bg", "warn_border": "warn-border", "warn_fg": "warn-fg",
}

var themeHexRe = regexp.MustCompile(`^#[0-9a-fA-F]{3}([0-9a-fA-F]{3}([0-9a-fA-F]{2})?)?$`)

// ThemeManifest 是解析后的主题包清单。
type ThemeManifest struct {
	ID          string
	Name        string
	Version     string
	Description string
	// Light/Dark 是色板覆盖（键为 CSS 变量后缀，如 "bg"、"accent"）。
	// 缺的键回落到内置默认——主题包只写想改的那几个。
	Light map[string]string
	Dark  map[string]string
	HasCSS bool
}

func parseThemePalette(tomlText, section string) (map[string]string, error) {
	out := map[string]string{}
	sec := extractTOMLSection(tomlText, section)
	for _, line := range strings.Split(sec, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("[theme.%s] 行不是 key = value: %q", section, line)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		cssKey, known := themePaletteKeys[k]
		if !known {
			return nil, fmt.Errorf("[theme.%s] 不支持的键 %q（允许的键见文档）", section, k)
		}
		if !themeHexRe.MatchString(v) {
			return nil, fmt.Errorf("[theme.%s] %s 的值 %q 不是合法颜色（要 #RGB/#RRGGBB/#RRGGBBAA）", section, k, v)
		}
		out[cssKey] = v
	}
	return out, nil
}

// extractTOMLSection 取 "[section]" 到下一个 "[" 顶格行之间的文本。
// 主题包的清单很小，不值得为此引入完整的 TOML 遍历。
func extractTOMLSection(text, section string) string {
	header := "[" + section + "]"
	var out []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			in = trimmed == header
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// ParseThemeManifest 解析主题包的 manifest.toml。
func ParseThemeManifest(raw []byte) (*ThemeManifest, error) {
	text := string(raw)
	getStr := func(key string) string {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, key+" ") || strings.HasPrefix(trimmed, key+"=") {
				_, v, ok := strings.Cut(trimmed, "=")
				if !ok {
					continue
				}
				return strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		return ""
	}
	if getStr("kind") != "theme" {
		return nil, fmt.Errorf("不是主题包（kind=%q，应为 \"theme\"）", getStr("kind"))
	}
	id := getStr("id")
	if !dirNameRe.MatchString(id) {
		return nil, fmt.Errorf("主题 id 不合法: %q", id)
	}
	light, err := parseThemePalette(text, "theme.light")
	if err != nil {
		return nil, err
	}
	dark, err := parseThemePalette(text, "theme.dark")
	if err != nil {
		return nil, err
	}
	return &ThemeManifest{
		ID:          id,
		Name:        getStr("name"),
		Version:     getStr("version"),
		Description: getStr("description"),
		Light:       light,
		Dark:        dark,
	}, nil
}

// InstallThemeZip 校验并安装主题包到 themesDir/<id>/（原子替换）。
// 信任闸门和插件安装同一条：坏公钥红线 → 签名验证 → 未签名仅开发模式。
// 返回主题 id。
func (i *Installer) InstallThemeZip(raw []byte, themesDir string) (string, error) {
	const maxThemePack = 8 << 20 // 8MB：主题就只有清单 + CSS + 图
	if len(raw) > maxThemePack {
		return "", fmt.Errorf("主题包超过 %d MB 上限", maxThemePack>>20)
	}
	// 坏公钥红线：和插件安装同一条，绝不静默进入不验签状态
	if i.keyErr != nil {
		return "", fmt.Errorf("可信公钥配置错误，拒绝安装: %w", i.keyErr)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("不是合法的 zip: %w", err)
	}

	id, err := peekZipID(zr)
	if err != nil {
		return "", err
	}

	// 白名单：主题包只允许这些内容，多余的东西直接拒绝——
	// 别让"主题"变成夹带任意文件的通道。
	// META-INF/ 是签名文件（签名包必带），提取时会被 extractZip 跳过
	for _, f := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(f.Name), "./")
		switch {
		case name == "manifest.toml" || name == "theme.css":
		case strings.HasPrefix(name, "assets/"):
		case strings.HasPrefix(name, "META-INF/"):
		case strings.HasSuffix(name, "/"):
		default:
			return "", fmt.Errorf("主题包里不允许的文件: %s（只允许 manifest.toml、theme.css、assets/、META-INF/）", name)
		}
	}

	// 信任闸门：带签名一定验签；没签名仅开发模式放行
	trusted := i.trustedKeys()
	if hasSignature(zr) {
		if err := verifyPackage(zr, trusted); err != nil {
			if fp, key := signerInfo(zr); fp != "" {
				return "", fmt.Errorf("签名校验失败: %w（签发者指纹 %s，公钥 %s）", err, fp, key)
			}
			return "", fmt.Errorf("签名校验失败: %w", err)
		}
	} else if !i.allowUnsigned {
		return "", fmt.Errorf("主题包没有签名（开发时可设 [plugin] allow_unsigned = true 放行）")
	}

	manData := readZipFileByName(zr, "manifest.toml")
	if manData == nil {
		return "", fmt.Errorf("包里缺少 manifest.toml")
	}
	man, err := ParseThemeManifest(manData)
	if err != nil {
		return "", err
	}
	if man.ID != id {
		return "", fmt.Errorf("包目录名 %s 和 manifest id %s 不一致", id, man.ID)
	}

	if err := os.MkdirAll(themesDir, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(themesDir, ".staging-"+id+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := extractZip(zr, staging, maxZipFile, maxZipTotal); err != nil {
		return "", err
	}

	target := filepath.Join(themesDir, id)
	old := ""
	if dirExists(target) {
		old = filepath.Join(themesDir, ".old-"+id+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		if err := renameRetry(target, old); err != nil {
			return "", fmt.Errorf("无法替换旧主题（可能被占用）: %w", err)
		}
	}
	if err := renameRetry(staging, target); err != nil {
		if old != "" {
			_ = renameRetry(old, target)
		}
		return "", err
	}
	if old != "" {
		removeAllRetry(old)
	}
	i.Log.Info("主题包已安装", "theme", id, "version", man.Version)
	return id, nil
}

func readZipFileByName(zr *zip.Reader, name string) []byte {
	for _, f := range zr.File {
		if strings.TrimPrefix(filepath.ToSlash(f.Name), "./") == name {
			return readZipFile(f, 1<<20)
		}
	}
	return nil
}
