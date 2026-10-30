package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Config 是框架的唯一配置入口。所有字段都有默认值，只有账号必须显式配置。
type Config struct {
	Storage     Storage     `toml:"storage"`
	Accounts    []Account   `toml:"accounts"`
	Command     Command     `toml:"command"`
	Plugin      Plugin      `toml:"plugin"`
	Web         Web         `toml:"web"`
	Log         Log         `toml:"log"`
	Owner       Owner       `toml:"owner"`
	Permissions Permissions `toml:"permissions"`
	Send        Send        `toml:"send"`
}

type Account struct {
	ID          string `toml:"id"`
	WSURL       string `toml:"ws_url"`
	AccessToken string `toml:"access_token"`
	HTTPURL     string `toml:"http_url"`
	HTTPToken   string `toml:"http_token"`
	SelfID      int64  `toml:"self_id"`
}

type Command struct {
	Prefixes []string `toml:"prefixes"`
	// Markdown 控制发送前的纯文本化：auto（默认，常见语法转成 QQ 能显示的样式）/ off
	Markdown string `toml:"markdown"`
}

type Plugin struct {
	// Dir 是包投放区：只放 .anzu
	Dir string `toml:"dir"`
	// RuntimeDir 是解压后的运行区，每个插件一个子目录
	RuntimeDir string `toml:"runtime_dir"`
	// StateDir 放状态、覆盖配置、日志
	StateDir string `toml:"state_dir"`

	HotReload bool `toml:"hot_reload"`
	TimeoutMS int  `toml:"timeout_ms"`

	// TrustedKeys 是 ed25519 公钥（base64）。为空表示不校验签名（开发模式）
	TrustedKeys []string `toml:"trusted_keys"`

	// NodeOptions 会作为 NODE_OPTIONS 传给插件进程。
	// 2G 小内存机器建议设成 "--max-old-space-size=96"，限制 Node 堆增长。
	NodeOptions string `toml:"node_options"`

	// CohostEnabled 打开共享运行时宿主：manifest 声明 cohost = true 的
	// 纯 JS 插件合并进一个 Node 进程（省内存，2G 小机友好）。
	// **默认开**（45 分钟浸泡 + 金丝雀演练通过后转正）；写 false 显式关闭。
	// 崩溃域为 worker 级——见 COHOST-DESIGN.md。
	CohostEnabled FlexBool `toml:"cohost_enabled"`
	// CohostGroupSize 是每组插件数上限（默认 6），控制爆炸半径。
	CohostGroupSize int `toml:"cohost_group_size"`
	// SDKDir 是插件 SDK 的位置，会被复制到每个插件的 _sdk/。
	// 留空时用 <config 所在目录>/sdk/node。
	SDKDir string `toml:"sdk_dir"`

	// AllowUnsigned 允许安装**未签名**的插件（开发用）。
	//
	// 配了 trusted_keys 之后默认只接受签名包；打开这个开关可以让
	// 自己写的插件（没签名）也能装，但会在日志里标出来。
	//
	// **签名无效的包始终拒绝**——这个开关只放行"没签名"，不放行"签错了"。
	AllowUnsigned bool `toml:"allow_unsigned"`

	// AutoEnable 让新安装的插件默认启用。
	// 生产环境应该保持 false（安装 ≠ 信任）；只有 qixinpack dev 会打开它。
	AutoEnable bool `toml:"auto_enable"`
	// Env 注入到每个插件进程，跨平台指定外部命令位置（如 FFMPEG_PATH）
	Env map[string]string `toml:"env"`
}

type Web struct {
	// TrustedProxies 是允许提供 X-Forwarded-For 的代理地址（IP 或 CIDR）。
	// 空 = 不信任任何转发头，只用 TCP 层的 RemoteAddr。
	// 部署在 nginx/Cloudflare 后面时，把代理地址填进来，否则限速会被伪造头绕过。
	TrustedProxies []string `toml:"trusted_proxies"`
	Enable         bool     `toml:"enable"`
	Addr           string   `toml:"addr"`
	// ShowTools 控制「工具」导航：auto（默认，有工具才显示）| always | never
	ShowTools string `toml:"show_tools"`
	Username  string `toml:"username"`
	Password  string `toml:"password"`
	// ThemePack 是控制台使用的主题包 id；空 = 内置默认配色。
	// 主题包经「连接」页上传（kind="theme" 的 .anzu，过同一套签名闸门）。
	ThemePack string `toml:"theme_pack"`
}

type Log struct {
	Level string `toml:"level"`
	File  string `toml:"file"`
	// GateDebug 打开后，被前缀/@/权限拦下的消息会记一条 debug 日志。
	// 默认关闭：正常使用时不刷屏，排查"为什么不理我"时再开。
	GateDebug bool `toml:"gate_debug"`
}

type Owner struct {
	Users []int64 `toml:"users"`
}

type Permissions struct {
	// Mode: allow_all（默认，装完就能用）| deny_all（默认拒绝，需显式授权）
	Mode string `toml:"mode"`
}

type Send struct {
	// IntervalMS 是同一个会话两条消息之间的最小间隔，防止触发风控。
	IntervalMS int `toml:"interval_ms"`
	// AllowGroups 非空时，插件只能往这些群发消息（空 = 不限）
	AllowGroups []int64 `toml:"allow_groups"`
}

func defaults() Config {
	return Config{
		Command: Command{Prefixes: []string{"/"}},
		Plugin: Plugin{
			Dir:        "plugins",
			RuntimeDir: "data/plugins",
			StateDir:   "data",
			HotReload:  false,
			TimeoutMS:  30000,
		},
		Web:         Web{Enable: true, Addr: "127.0.0.1:7801", Username: "admin"},
		Log:         Log{Level: "info"},
		Permissions: Permissions{Mode: "allow_all"},
		Send:        Send{IntervalMS: 300},
	}
}

// Load 读取配置文件，补默认值并校验。
func Load(path string) (*Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		if _, err := toml.Decode(string(data), &cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	d := defaults()
	if len(c.Command.Prefixes) == 0 {
		c.Command.Prefixes = d.Command.Prefixes
	}
	if c.Plugin.Dir == "" {
		c.Plugin.Dir = d.Plugin.Dir
	}
	if c.Plugin.RuntimeDir == "" {
		c.Plugin.RuntimeDir = d.Plugin.RuntimeDir
	}
	if c.Plugin.StateDir == "" {
		c.Plugin.StateDir = d.Plugin.StateDir
	}
	if c.Plugin.TimeoutMS <= 0 {
		c.Plugin.TimeoutMS = d.Plugin.TimeoutMS
	}
	if c.Web.Addr == "" {
		c.Web.Addr = d.Web.Addr
	}
	if c.Log.Level == "" {
		c.Log.Level = d.Log.Level
	}
	if c.Permissions.Mode == "" {
		c.Permissions.Mode = d.Permissions.Mode
	}
	if c.Send.IntervalMS <= 0 {
		c.Send.IntervalMS = d.Send.IntervalMS
	}
}

func (c *Config) Validate() error {
	if len(c.Accounts) == 0 {
		return fmt.Errorf("配置里至少需要一个 [[accounts]]")
	}
	ids := map[string]bool{}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.ID == "" {
			a.ID = fmt.Sprintf("acc%d", i+1)
		}
		if ids[a.ID] {
			return fmt.Errorf("账号 id 重复: %s", a.ID)
		}
		ids[a.ID] = true
		if a.WSURL == "" {
			return fmt.Errorf("账号 %s 缺少 ws_url（SnowLuma 默认 ws://127.0.0.1:3001）", a.ID)
		}
		if !strings.HasPrefix(a.WSURL, "ws://") && !strings.HasPrefix(a.WSURL, "wss://") {
			return fmt.Errorf("账号 %s 的 ws_url 必须以 ws:// 或 wss:// 开头", a.ID)
		}
	}
	if c.Permissions.Mode != "allow_all" && c.Permissions.Mode != "deny_all" {
		return fmt.Errorf("permissions.mode 只能是 allow_all 或 deny_all")
	}
	return nil
}

// Abs 把相对路径解析到项目根目录下。
func Abs(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

// saveMu 串行化配置写盘：多个管理页面同时保存时不能共用同一个临时文件。
var saveMu sync.Mutex

// Save 把配置写回 TOML 文件。先写临时文件再 rename，避免写一半断电。
func Save(path string, c *Config) error {
	saveMu.Lock()
	defer saveMu.Unlock()

	var b strings.Builder
	enc := toml.NewEncoder(&b)
	if err := enc.Encode(c); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是空操作
	_ = tmp.Chmod(0o600)
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// fsync 后再 rename，否则掉电时 rename 先落、数据还在缓存里，
	// "避免写一半断电"就只是心理安慰
	if f, err := os.Open(tmpName); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600) // 覆盖已有文件时也收窄权限
	return nil
}

// Storage 决定持久化用哪个后端。
type Storage struct {
	// Driver: "sqlite"（默认）| "json"（一个文件，适合本地调试）
	Driver string `toml:"driver"`
	// Path 是数据文件路径，相对工作目录。留空按 driver 取默认值。
	Path string `toml:"path"`
}

// DriverOrDefault 返回生效的驱动名，空值按 sqlite（默认）。
func (s Storage) DriverOrDefault() string {
	if s.Driver == "" {
		return "sqlite"
	}
	return s.Driver
}

// StoragePath 返回实际的数据库路径。
func (s Storage) StoragePath(root string) string {
	if s.Path != "" {
		if filepath.IsAbs(s.Path) {
			return s.Path
		}
		return filepath.Join(root, s.Path)
	}
	if s.DriverOrDefault() == "sqlite" {
		return filepath.Join(root, "data", "qixin.db")
	}
	return filepath.Join(root, "data", "store.json")
}

// FlexBool 是"缺省为 true"的布尔配置项：没写这个键 = true，
// 显式写了（true/false）以写了的为准。BurntSushi 对缺过的字段不会调
// UnmarshalTOML，所以零值必须是"未设置"，由 Bool() 给出缺省。
type FlexBool struct {
	Set bool
	V   bool
}

// UnmarshalTOML 实现 BurntSushi 的 Unmarshaler。
func (f *FlexBool) UnmarshalTOML(v any) error {
	if b, ok := v.(bool); ok {
		f.Set = true
		f.V = b
	}
	return nil
}

// MarshalTOML 落盘写**生效值**：没设置过的键序列化成 true。
// 直接写 V 的话，WebUI 保存任意配置（改密码/主人）都会把没写过的
// cohost_enabled 落成 false，下次重启共享宿主被静默关掉。
func (f FlexBool) MarshalTOML() ([]byte, error) {
	if f.Bool() {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

// Bool 返回生效值：未设置 = true。
func (f FlexBool) Bool() bool { return !f.Set || f.V }
