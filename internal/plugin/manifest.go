package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/xin/onebot/internal/perm"
	"github.com/xin/onebot/internal/registry"
)

// Manifest 是 manifest.toml 的结构。它是插件对外的唯一声明，也是 help / 权限 /
// 工具列表的数据来源。
type Manifest struct {
	ID          string   `toml:"id"`
	Name        string   `toml:"name"`
	Version     string   `toml:"version"`
	APIVersion  string   `toml:"apiVersion"`
	Exec        []string `toml:"exec"`
	Description string   `toml:"description"`
	Enabled     *bool    `toml:"enabled"`

	// Cohost = true 声明本插件可进共享宿主（与其它 cohost 插件共用一个
	// Node 进程）。只对纯 JS 插件有意义；[plugin] cohost_enabled 打开后生效。
	Cohost bool `toml:"cohost"`
	// Priority 大的先收到消息；默认 0
	Priority int `toml:"priority"`
	// CapabilityPriority 是"作为能力提供者"的优先级；不写时沿用 Priority。
	// 拆开是因为"消息分发靠后"和"能力优先级低"不是一回事。
	CapabilityPriority *int `toml:"capability_priority"`
	// Last 为 true 表示兜底插件（例如 AI 聊天），永远排在普通插件之后
	Last bool `toml:"last"`
	// UseTools 表示这个插件会给模型提供工具列表（AI 聊天类）。
	// 为 true 时，插件详情页会显示工具面板；框架导航里不再单独出「工具」页。
	UseTools bool `toml:"use_tools"`

	// Observe 表示"这条消息不管谁处理都要给我一份"（记忆、统计类插件用）。
	// 框架会调 plugin.observe_message，忽略返回值，不影响回复。
	Observe bool `toml:"observe"`

	// Events 是订阅的非消息事件类型：post_type（notice/request/meta_event）
	// 或 "notice:group_upload" 这种细分；"*" 表示全部。
	// 只有声明的插件才会收到对应的 handle_event。
	Events []string `toml:"events"`

	// Assets 声明用到的素材子目录（标准目录不用写）。
	Assets AssetsSpec `toml:"assets"`

	// Kind 为 "service" 表示这是基础设施插件（不接消息，只提供能力）。
	// 分发时会跳过它，WebUI 单独归类。
	Kind string `toml:"kind"` // ""（普通插件）| "service"

	// Concurrency 决定消息处理粒度：
	//   "plugin"（默认）— 整个插件串行，最安全，现有插件都按这个假设写的
	//   "session"       — 同一会话串行、不同会话并发，适合游戏/多人在线
	Concurrency string `toml:"concurrency"`

	// Web 是设置页入口，相对运行区根目录，例如 "web/index.html"
	Web string `toml:"web"`
	// ClosedSource 只是标记：让管理员知道这个包不含源码
	ClosedSource bool `toml:"closed_source"`

	// Access 是插件级权限：一个插件一套控制，命令级可再覆盖
	Access Access `toml:"access"`

	Permissions  []string         `toml:"permissions"`
	Provides     []string         `toml:"provides"`
	Uses         []string         `toml:"uses"`
	Commands     []CommandSpec    `toml:"commands"`
	Tools        []ToolSpec       `toml:"tools"`
	Settings     []SettingSpec    `toml:"settings"`
	Shared       []SharedSpec     `toml:"shared"`
	Capabilities []CapabilitySpec `toml:"capability"`
	Cooldown     CooldownSpec     `toml:"cooldown"`
	Timeouts     TimeoutsSpec     `toml:"timeouts"`

	// LoadOrder 决定加载顺序：**小的先加载**（默认 0）。
	// 同 order 按 id 字母序。
	//
	// 什么时候用：服务插件初始化时要调另一个服务，被调的那个
	// 可以设成更小的值（比如 10），保证它先起来。
	//
	// 注意和 [[commands]].priority 区分：那个是"消息分发给谁先"，
	// 这个是"启动顺序"。
	LoadOrder int           `toml:"load_order"`
	Runtime   *RuntimeSpec  `toml:"runtime"`
	Requires  []RequireSpec `toml:"requires"`
}

// Access 是插件级的访问控制。WebUI 的「权限」页按插件配置的就是这套。
type Access struct {
	AllowUsers  []int64 `toml:"allow_users"`
	AllowGroups []int64 `toml:"allow_groups"`
	RequireRole string  `toml:"require_role"`

	// Args 声明位置参数（按 usage 里 <xx> 的顺序）。
	// 声明后宿主会解析好塞进 session.Command.Args，插件不用自己切字符串。
	Args map[string]ArgSpec `toml:"args"`

	RequirePrefix  *bool `toml:"require_prefix"`
	RequireMention *bool `toml:"require_mention"`
}

// CommandSpec 是单条命令。权限字段是"命令级覆盖"，不写就用插件级。
type CommandSpec struct {
	ID          string   `toml:"id"`
	Usage       string   `toml:"usage"`
	Description string   `toml:"description"`
	Hidden      bool     `toml:"hidden"`
	Aliases     []string `toml:"aliases"`

	AllowUsers  []int64 `toml:"allow_users"`
	AllowGroups []int64 `toml:"allow_groups"`
	RequireRole string  `toml:"require_role"`

	// Args 声明位置参数（按 usage 里 <xx> 的顺序）。
	// 声明后宿主会解析好塞进 session.Command.Args，插件不用自己切字符串。
	Args map[string]ArgSpec `toml:"args"`

	// RequirePrefix 默认 true：命令必须带 / 前缀
	RequirePrefix *bool `toml:"require_prefix"`
	// RequireMention 默认 false：群里必须 @ 机器人
	RequireMention *bool `toml:"require_mention"`
}

type ToolSpec struct {
	Name        string         `toml:"name"`
	Description string         `toml:"description"`
	Parameters  map[string]any `toml:"parameters"`
}

// RequireSpec 是插件对外部依赖的声明（ffmpeg、sqlite3 这类系统命令）。
// 和 [runtime] 的区别：runtime 管解释器，requires 管插件要调用的工具。
type RequireSpec struct {
	Name     string   `toml:"name"`     // 展示名
	Kind     string   `toml:"kind"`     // binary（默认）| env | file | plugin | python | node
	Value    string   `toml:"value"`    // binary: 命令名；env: 变量名；file: 路径
	Hint     string   `toml:"hint"`     // 缺失时给管理员的提示
	Module   string   `toml:"module"`   // python: import 名（如 Pillow -> PIL）
	Version  string   `toml:"version"`  // python/node: 兼容范围（记录用）
	Download bool     `toml:"download"` // 预留：允许安装时下载
	Hashes   []string `toml:"hashes"`   // 预留：下载哈希
}

// RuntimeSpec 是插件对运行环境的要求。不写时框架会从 exec[0] 自动推断。
// AssetsSpec 声明插件用到的素材子目录。
// img / audio / video / fonts / web 是标准目录，不用声明；
// 自己另开的子目录写在这里，框架会校验它存在（不存在只警告，不阻止加载）。
type AssetsSpec struct {
	Dirs []string `toml:"dirs"`
}

// StandardAssetDirs 是框架认的标准素材子目录。
var StandardAssetDirs = []string{"img", "audio", "video", "fonts", "web"}

type RuntimeSpec struct {
	Kind    string `toml:"kind"`    // node / python / binary（可选，用于覆盖推断）
	Version string `toml:"version"` // ">=18"，可选

	// Platform 限制插件只在特定系统加载。
	// 空 = 全平台；也可写 "windows" / "linux" / "darwin"，多个用 | 分隔。
	// 带原生依赖（.node / .so）的插件必须写，否则换平台会启动失败。
	Platform string `toml:"platform"`
}

// CapabilitySpec 是能力的详细声明：名字 + 参数 schema。
// 只有写了这个，宿主才会校验调用参数；只写 provides 的话参数不做检查。
type CapabilitySpec struct {
	Name       string         `toml:"name"`
	Parameters map[string]any `toml:"parameters"`
	// MaxConcurrent 限制同时有多少个调用打向这个提供者。
	// 0 = 不限。Python 服务建议设成 1（它是同步逐条处理的）。
	MaxConcurrent int `toml:"max_concurrent"`

	// Retry 决定调用失败后要不要换下一个提供者。
	//   空 / "safe"（默认）：只重试"提供者根本没运行"的情况
	//   "always"：超时/内部错误也重试（**要求能力幂等**）
	Retry string `toml:"retry"`
}

// SharedSpec 是插件声明的一份共享数据，存在框架侧，其他插件可读。
type SharedSpec struct {
	Key         string `toml:"key"`
	Description string `toml:"description"`
	Default     any    `toml:"default"`
}

type SettingSpec struct {
	Key     string   `toml:"key" json:"key"`
	Label   string   `toml:"label" json:"label"`
	Type    string   `toml:"type" json:"type"`
	Default any      `toml:"default" json:"default,omitempty"`
	Options []string `toml:"options" json:"options,omitempty"`
	// OptionLabels 是下拉项的中文名，和 Options 一一对应（缺省就用 Options 本身）
	OptionLabels []string `toml:"option_labels" json:"option_labels,omitempty"`
	// Hint 是字段下方的说明文字（怎么填、格式是什么）
	Hint string   `toml:"hint" json:"hint,omitempty"`
	Min  *float64 `toml:"min" json:"min,omitempty"`
	Max  *float64 `toml:"max" json:"max,omitempty"`
}

var dirNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,40}$`)

// knownSettingTypes 是设置项支持的类型。写错会退化成文本框，
// 所以加载时统一喊一声，别等用户在页面上发现。
var knownSettingTypes = map[string]bool{
	"text": true, "string": true, "number": true, "boolean": true, "bool": true,
	"select": true, "password": true,
}

// Duration 是 manifest 里的时长字符串（"10m" / "2h"）。
type Duration time.Duration

// UnmarshalText 让 toml 能把 "10m" 解析成 Duration。
func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" || s == "0" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("时长格式不对（要 10m / 2h 这种）: %w", err)
	}
	*d = Duration(v)
	return nil
}

// D 返回 time.Duration。
func (d Duration) D() time.Duration { return time.Duration(d) }

// LoadManifest 读取并校验 <dir>/manifest.toml。
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "manifest.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	var m Manifest
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if m.ID == "" {
		m.ID = filepath.Base(dir)
	}
	if err := m.validate(filepath.Base(dir)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.Version == "" {
		m.Version = "0.0.0"
	}
	if m.APIVersion == "" {
		m.APIVersion = "1.x"
	}

	// 剥掉 usage 里手写的前缀。有的作者会写 "/cngal"，但匹配前框架已经
	// 去掉过一个前缀了，不剥掉就永远对不上（配了前缀时表现为插件完全不触发）。
	for i := range m.Commands {
		m.Commands[i].Usage = stripPrefixSymbol(m.Commands[i].Usage)
		for j := range m.Commands[i].Aliases {
			m.Commands[i].Aliases[j] = stripPrefixSymbol(m.Commands[i].Aliases[j])
		}
	}

	return &m, nil
}

// stripPrefixSymbol 去掉命令名开头的一个前缀符号（/ ! # 等）。
// 只去一个，避免把 "a/b" 这种合法内容改坏。
func stripPrefixSymbol(usage string) string {
	u := strings.TrimSpace(usage)
	if u == "" {
		return u
	}
	switch u[0] {
	case '/', '!', '#', '.':
		return strings.TrimSpace(u[1:])
	}
	return u
}

func (m *Manifest) validate(dirName string) error {
	if !dirNameRe.MatchString(m.ID) {
		return fmt.Errorf("插件 id %q 不合法，需匹配 %s", m.ID, dirNameRe.String())
	}
	if m.ID != dirName {
		return fmt.Errorf("manifest.id=%q 与目录名 %q 不一致", m.ID, dirName)
	}
	if len(m.Exec) == 0 || m.Exec[0] == "" {
		return fmt.Errorf("缺少 exec，例如 exec = [\"node\", \"index.js\"]")
	}
	if major(m.APIVersion) != 1 {
		return fmt.Errorf("不支持的 apiVersion %q（本框架支持 1.x）", m.APIVersion)
	}
	for _, c := range m.Commands {
		if c.ID == "" {
			return fmt.Errorf("commands 里有条目缺少 id")
		}
		if !registry.ValidID(c.ID) {
			return fmt.Errorf("命令 id %q 不合法，必须形如 plugin.verb", c.ID)
		}
	}
	for _, t := range m.Tools {
		if t.Name == "" {
			return fmt.Errorf("tools 里有条目缺少 name")
		}
	}
	return nil
}

// Features 把 manifest 里的命令和工具展开成注册表条目。
func (m *Manifest) Features() []*registry.Feature {
	out := make([]*registry.Feature, 0, len(m.Commands)+len(m.Tools))
	for _, c := range m.Commands {
		out = append(out, &registry.Feature{
			ID:          c.ID,
			Plugin:      m.ID,
			Kind:        registry.KindCommand,
			Label:       m.Name,
			Usage:       c.Usage,
			Description: c.Description,
			Hidden:      c.Hidden,
			Policy: perm.Policy{
				AllowUsers:  c.AllowUsers,
				AllowGroups: c.AllowGroups,
				RequireRole: c.RequireRole,
			},
			RequirePrefix:  boolOr(c.RequirePrefix, true),
			RequireMention: boolOr(c.RequireMention, false),
		})
	}
	for _, t := range m.Tools {
		out = append(out, &registry.Feature{
			ID:          "tool." + t.Name,
			Plugin:      m.ID,
			Kind:        registry.KindTool,
			Label:       m.Name,
			Usage:       t.Name,
			Description: t.Description,
		})
	}
	return out
}

// Enabled 返回插件是否启用（默认启用）。
func (m *Manifest) EnabledOr(defaultValue bool) bool {
	if m.Enabled == nil {
		return defaultValue
	}
	return *m.Enabled
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func major(v string) int {
	n := 0
	for _, r := range v {
		if r >= '0' && r <= '9' {
			// 长度封顶防整型回绕：18446744073709551617 之前能伪装成 1 过闸
			if n > 999999999 {
				return -1
			}
			n = n*10 + int(r-'0')
			continue
		}
		break
	}
	return n
}

// TimeoutsSpec 让重依赖的插件放宽超时。
//
// 默认：initialize 10 秒、能力调用 15 秒。
// 加载大模型、连慢速后端的服务插件可以放宽；但别调到 10 分钟以上，
// 那期间调用方一直在等。
type TimeoutsSpec struct {
	Init       string `toml:"init"`       // 启动握手
	Capability string `toml:"capability"` // 能力调用
	Message    string `toml:"message"`    // 单条消息处理（默认 30 秒）
	Call       string `toml:"call"`       // 单次宿主调用兜底上限（默认 30 分钟）
}

// InitTimeout 返回启动超时（0 = 用默认）。
func (t TimeoutsSpec) InitTimeout() time.Duration {
	if d, err := time.ParseDuration(t.Init); err == nil {
		return d
	}
	return 0
}

// CapabilityTimeout 返回能力调用超时（0 = 用默认）。
func (t TimeoutsSpec) CapabilityTimeout() time.Duration {
	if d, err := time.ParseDuration(t.Capability); err == nil {
		return d
	}
	return 0
}

// CallTimeout 返回单次宿主调用的兜底上限（0 = 用默认 30 分钟）。
//
// 注意：这不是"业务超时"——host.ask 的等待时长由它自己的 timeout_sec 决定，
// 这里只是防止某个方法挂死永远不返回。
func (t TimeoutsSpec) CallTimeout() time.Duration {
	if d, err := time.ParseDuration(t.Call); err == nil {
		return d
	}
	return 0
}

// MessageTimeout 返回消息处理超时（0 = 用配置里的全局值）。
func (t TimeoutsSpec) MessageTimeout() time.Duration {
	if d, err := time.ParseDuration(t.Message); err == nil {
		return d
	}
	return 0
}

// CooldownSpec 是插件级的命令冷却（宿主统一执行）。
//
// 冷却期内**静默跳过**，不回"慢点"——刷屏时每条都回一句更吵。
type CooldownSpec struct {
	PerUser   string `toml:"per_user"`   // 同一用户对同一插件的间隔
	PerGroup  string `toml:"per_group"`  // 同一群的间隔
	PerPlugin string `toml:"per_plugin"` // 整个插件的全局间隔
}

// toConfig 把字符串解析成时长；解析失败按不限制处理。
func (c CooldownSpec) toConfig() CooldownConfig {
	var out CooldownConfig
	if d, err := time.ParseDuration(c.PerUser); err == nil {
		out.PerUser = d
	}
	if d, err := time.ParseDuration(c.PerGroup); err == nil {
		out.PerGroup = d
	}
	if d, err := time.ParseDuration(c.PerPlugin); err == nil {
		out.PerPlugin = d
	}
	return out
}

// ArgSpec 描述一个位置参数。
type ArgSpec struct {
	Type     string `toml:"type"` // string | int | rest（rest = 吃掉剩余全部）
	Required bool   `toml:"required"`
	Help     string `toml:"help"`
}
