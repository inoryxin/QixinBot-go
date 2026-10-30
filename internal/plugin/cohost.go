package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SharedHost 是共享运行时宿主的 Go 侧句柄：一个 multi-host 子进程
// 带着多个插件 worker（每插件一个线程）。
//
// 实验阶段（cohost 分支）：不接 Manager 生命周期，只提供与 Host.Call
// 同形的调用面，供 M2 集成与测试。
//
// 路由键：宿主 → 子进程的请求带 plugin 字段；子进程 → 宿主的响应/通知
// 也带 plugin。pending 表按 (plugin, id) 复合键登记。
type SharedHost struct {
	cfgPath string // multi-host 的 plugins.json
	log     *slog.Logger

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	pendMu  sync.Mutex
	pending map[string]chan *Response

	// handler 处理插件发来的 host.* 调用（多路复用：带 pluginID）
	handler func(ctx context.Context, pluginID, method string, params json.RawMessage) (any, *RPCError)

	done    chan struct{}
	exitErr error
	closing atomic.Bool

	writeMu sync.Mutex
	seq     atomic.Int64

	workerMu    sync.Mutex
	workerAlive map[string]bool

	memberMu   sync.Mutex
	members    map[string]CohostMember
	maxMembers int // 组员上限（0 = 不限）；配置 cohost_group_size
	// onWorkerExit 供 Manager 做崩溃计数/自动禁用（worker 级，进程级走 supervise）
	onWorkerExit func(pluginID string, code int)
}

// CohostMember 描述共享组里的一个插件。
type CohostMember struct {
	ID       string          `json:"id"`
	Dir      string          `json:"dir"`                // 插件运行目录（含 app/、_sdk/）
	Entry    string          `json:"entry"`              // 相对 dir 的入口，默认 app/index.js
	DataDir  string          `json:"data_dir,omitempty"` // 插件私有数据目录（initialize 契约字段）
	Settings json.RawMessage `json:"settings,omitempty"` // initialize 用的设置（重启自动重发）
}

// NewSharedHostConfig 把成员写成 multi-host 的 plugins.json。
// sdkDir 指向 sdk/node（worker-entry.mjs 所在目录）。
func NewSharedHostConfig(dir, sdkDir string, members []CohostMember) (string, error) {
	if members == nil {
		members = []CohostMember{}
	}
	cfg := struct {
		SDKDir  string         `json:"sdkDir"`
		Plugins []CohostMember `json:"plugins"`
	}{SDKDir: filepath.ToSlash(sdkDir), Plugins: members}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "cohost-plugins.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// SharedHostOptions 是共享宿主进程的启动参数。
type SharedHostOptions struct {
	NodeExe  string   // node 解释器（RuntimeOverride 或 PATH 留空）
	ExtraEnv []string // [plugin] env / NODE_PATH 等（KEY=VALUE）
}

// StartSharedHost 拉起 multi-host 子进程。
func StartSharedHost(multiHostPath, cfgPath string, opts SharedHostOptions, handler func(ctx context.Context, pluginID, method string, params json.RawMessage) (any, *RPCError), log *slog.Logger) (*SharedHost, error) {
	nodeExe := opts.NodeExe
	if nodeExe == "" {
		nodeExe = "node"
	}
	cmd := exec.Command(nodeExe, multiHostPath, cfgPath)
	if len(opts.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), opts.ExtraEnv...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	sh := &SharedHost{
		cfgPath:     cfgPath,
		log:         log,
		cmd:         cmd,
		stdin:       stdin,
		pending:     make(map[string]chan *Response),
		handler:     handler,
		done:        make(chan struct{}),
		workerAlive: make(map[string]bool),
		members:     make(map[string]CohostMember),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 multi-host: %w", err)
	}
	go sh.readLoop(stdout)
	go func() {
		sh.exitErr = cmd.Wait()
		close(sh.done)
	}()
	go sh.drainStderr(stderr)
	return sh, nil
}

func (sh *SharedHost) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	for sc.Scan() {
		sh.log.Info("multi-host: " + sc.Text())
	}
}

// Done 在 multi-host 进程退出时关闭。
func (sh *SharedHost) Done() <-chan struct{} { return sh.done }

// PID 返回共享宿主 Node 进程 PID；未启动/已退出返回 0。
func (sh *SharedHost) PID() int {
	if sh == nil || sh.cmd == nil || sh.cmd.Process == nil {
		return 0
	}
	return sh.cmd.Process.Pid
}

// RSSMB 返回共享宿主进程 RSS（MB）；不可用时返回 0。
// 共享组里的每个插件都显示这个进程级值，不能按插件相加。
func (sh *SharedHost) RSSMB() float64 {
	pid := sh.PID()
	if pid == 0 {
		return 0
	}
	return processRSSMB(pid)
}

func (sh *SharedHost) write(frame map[string]any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return sh.writeRaw(b)
}

func (sh *SharedHost) writeRaw(b []byte) error {
	sh.writeMu.Lock()
	defer sh.writeMu.Unlock()
	if sh.closing.Load() {
		return errors.New("共享宿主已关闭")
	}
	// 带超时的写：multi-host 主线程挂死时管道会被填满，
	// 无超时的 Write 会永久阻塞——全组插件"ready 但全哑"。
	// 超时 = 判死共享进程，Kill 后 ensureSharedHost 走重建。
	done := make(chan error, 1)
	go func() {
		_, werr := sh.stdin.Write(append(b, '\n'))
		done <- werr
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		if sh.cmd != nil && sh.cmd.Process != nil {
			_ = sh.cmd.Process.Kill() // done 关闭 → 各调用方快速失败 → 重建
		}
		return errors.New("写共享宿主超时（进程可能挂死），已终止等待重建")
	}
}

// Call 向指定插件发起一次请求（等待响应）。
func (sh *SharedHost) Call(ctx context.Context, pluginID, method string, params any) (json.RawMessage, error) {
	id := "h-" + strconv.FormatInt(sh.seq.Add(1), 10)
	key := pluginID + "/" + id
	ch := make(chan *Response, 1)
	sh.pendMu.Lock()
	sh.pending[key] = ch
	sh.pendMu.Unlock()
	defer func() {
		sh.pendMu.Lock()
		delete(sh.pending, key)
		sh.pendMu.Unlock()
	}()

	frame := map[string]any{"jsonrpc": JSONRPCVersion, "id": id, "plugin": pluginID, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if err := sh.write(frame); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-sh.done:
		return nil, errors.New("共享宿主进程已退出")
	}
}

// failPendingOf 把某插件名下所有 pending 调用以错误收场（worker 退出时）。
func (sh *SharedHost) failPendingOf(pluginID string) {
	prefix := pluginID + "/"
	sh.pendMu.Lock()
	defer sh.pendMu.Unlock()
	for key, ch := range sh.pending {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		delete(sh.pending, key)
		select {
		case ch <- &Response{JSONRPC: JSONRPCVersion, Error: Errf(CodeUnavailable, "插件 worker 已退出")}:
		default:
		}
	}
}

// EnsureMemberAndWait 把插件加进共享组（动态成员），等 worker 就绪并完成
// plugin.initialize（带安装时存的 settings）。worker 自动重启后由 §__worker_exit
// 的重初始化逻辑接管，不需要调用方再走一遍。
func (sh *SharedHost) EnsureMemberAndWait(ctx context.Context, m CohostMember) error {
	// 总超时兜底：startCohost 的调用方多持有 reloadMu（supervise/Reload/
	// ensureRunning），ctx 常无 deadline——worker 起不来的话无上限的
	// WaitPluginReady 会把整个管理器卡死（P0）
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	sh.memberMu.Lock()
	_, already := sh.members[m.ID]
	if !already && sh.maxMembers > 0 && len(sh.members) >= sh.maxMembers {
		sh.memberMu.Unlock()
		return ErrCohostFull
	}
	sh.members[m.ID] = m
	sh.memberMu.Unlock()
	// 新注册的成员在任何一步失败后都要摘掉：
	// 否则失败的 worker 永久占一个组员名额，组满后别的插件进不来
	newMember := !already
	dropMember := func() {
		if !newMember {
			return
		}
		sh.memberMu.Lock()
		delete(sh.members, m.ID)
		sh.memberMu.Unlock()
	}
	if err := sh.write(map[string]any{
		"__ctl": "start", "plugin": m.ID, "dir": m.Dir, "entry": m.Entry, "data_dir": m.DataDir,
	}); err != nil {
		dropMember()
		return err
	}
	if err := sh.WaitPluginReady(ctx, m.ID); err != nil {
		// worker 可能已经被 __ctl start 拉起来空转吃内存（initialize
		// 还没跑过）：显式停掉。StopPlugin 也会摘成员登记。
		_ = sh.StopPlugin(m.ID)
		dropMember()
		return fmt.Errorf("worker 未就绪: %w", err)
	}
	settings := m.Settings
	if len(settings) == 0 {
		settings = json.RawMessage("{}")
	}
	if _, err := sh.Call(ctx, m.ID, "plugin.initialize", map[string]any{
		"plugin_id":    m.ID,
		"api_version":  "1.0",
		"host_version": "0.1.0",
		"data_dir":     m.DataDir,
		"settings":     settings,
	}); err != nil {
		// 初始化失败的 worker 还活着（空转占内存）：按指令停掉
		_ = sh.StopPlugin(m.ID)
		dropMember()
		return fmt.Errorf("initialize 失败: %w", err)
	}
	return nil
}

// StopPlugin 指令停掉某个插件 worker（禁用/删除时用，不自动重启）。
// 同时摘掉组员登记：否则禁用后的插件还占着 cohost_group_size 名额，
// 新插件进组会被误判"组满"。重新启用走 EnsureMemberAndWait 重新登记。
func (sh *SharedHost) StopPlugin(pluginID string) error {
	sh.workerMu.Lock()
	sh.workerAlive[pluginID] = false
	sh.workerMu.Unlock()
	sh.memberMu.Lock()
	delete(sh.members, pluginID)
	sh.memberMu.Unlock()
	return sh.write(map[string]any{"__ctl": "stop", "plugin": pluginID})
}

// StartPlugin 拉起某个插件 worker（enable / 崩溃后手动拉起）。
func (sh *SharedHost) StartPlugin(pluginID string) error {
	sh.workerMu.Lock()
	sh.workerAlive[pluginID] = true
	sh.workerMu.Unlock()
	return sh.write(map[string]any{"__ctl": "start", "plugin": pluginID})
}

// ReloadPlugin 重载某个插件 worker（自动重启会拿到新代码）。
func (sh *SharedHost) ReloadPlugin(pluginID string) error {
	return sh.write(map[string]any{"__ctl": "reload", "plugin": pluginID})
}

// WaitPluginReady 等 worker 起来并能应答（ping 探测循环）。
// worker 自动重启后（拿到新代码）需要重新 initialize——调用方在 ready 后
// 自行发送 plugin.initialize。
func (sh *SharedHost) WaitPluginReady(ctx context.Context, pluginID string) error {
	for {
		if sh.closing.Load() {
			return errors.New("共享宿主已关闭")
		}
		err := sh.PingPlugin(ctx, pluginID)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sh.done:
			return errors.New("共享宿主进程已退出")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// UpdateMemberSettings 刷新成员的设置快照（set_settings 推送时调用）——
// worker 崩溃自动重启的重初始化要用最新值，不然配置悄悄回退。
func (sh *SharedHost) UpdateMemberSettings(pluginID string, settings json.RawMessage) {
	sh.memberMu.Lock()
	defer sh.memberMu.Unlock()
	if m, ok := sh.members[pluginID]; ok {
		m.Settings = settings
		sh.members[pluginID] = m
	}
}

// NotifyPlugin 向单个插件发 notification（无 id，不等待）。
func (sh *SharedHost) NotifyPlugin(pluginID, method string, params any) error {
	frame := map[string]any{"jsonrpc": JSONRPCVersion, "method": method, "plugin": pluginID}
	if params != nil {
		frame["params"] = params
	}
	return sh.write(frame)
}

// PingPlugin 对单个插件 ping。
func (sh *SharedHost) PingPlugin(ctx context.Context, pluginID string) error {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := sh.Call(c, pluginID, "plugin.ping", map[string]any{})
	return err
}

func (sh *SharedHost) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		sh.handleLine(line)
	}
}

func (sh *SharedHost) handleLine(line []byte) {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Plugin string          `json:"plugin"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		sh.log.Warn("multi-host 输出非 JSON 行", "line", string(line[:min(len(line), 200)]))
		return
	}

	// 内部控制帧
	var internal struct {
		WorkerExit bool   `json:"__worker_exit"`
		Plugin     string `json:"plugin"`
		Code       int    `json:"code"`
		Stopped    bool   `json:"stopped"`
	}
	if json.Unmarshal(line, &internal) == nil && internal.WorkerExit {
		sh.workerMu.Lock()
		sh.workerAlive[internal.Plugin] = false
		sh.workerMu.Unlock()
		sh.log.Warn("共享组 worker 退出", "plugin", internal.Plugin, "code", internal.Code,
			"stopped", internal.Stopped)
		// 该 worker 名下所有 pending 调用立刻失败（设计 §4.5）——
		// 不然调用方要挂到自己的超时
		sh.failPendingOf(internal.Plugin)
		if sh.onWorkerExit != nil && !internal.Stopped {
			// 异步：onWorkerExit 会走 Manager.Disable（拿 reloadMu），
			// 而本 goroutine 是唯一的响应路由——同步调 = 死锁风险
			go sh.onWorkerExit(internal.Plugin, internal.Code)
		}
		// 非指令停止 = 崩溃：multi-host 会自动重启，重启后的 worker 需要
		// 重新 initialize（用安装时存的 settings）——这就是"唤不醒"问题的
		// 解法：不依赖一次性握手，重试循环 + 自动重初始化
		if !internal.Stopped {
			sh.memberMu.Lock()
			member, ok := sh.members[internal.Plugin]
			sh.memberMu.Unlock()
			if ok {
				go func() {
					time.Sleep(600 * time.Millisecond)
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					if err := sh.WaitPluginReady(ctx, internal.Plugin); err != nil {
						sh.log.Warn("共享组 worker 重启后未就绪", "plugin", internal.Plugin, "err", err)
						return
					}
					settings := member.Settings
					if len(settings) == 0 {
						settings = json.RawMessage("{}")
					}
					if _, err := sh.Call(ctx, internal.Plugin, "plugin.initialize", map[string]any{
						"plugin_id":    internal.Plugin,
						"api_version":  "1.0",
						"host_version": "0.1.0",
						"data_dir":     member.DataDir,
						"settings":     settings,
					}); err != nil {
						sh.log.Warn("共享组 worker 重初始化失败", "plugin", internal.Plugin, "err", err)
					} else {
						sh.log.Info("共享组 worker 已恢复", "plugin", internal.Plugin)
					}
				}()
			}
		}
		return
	}

	if probe.Method != "" {
		// 插件（worker）发来的 host.* 请求：复用宿主处理器
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			var result any = map[string]any{}
			var rerr *RPCError
			if sh.handler == nil {
				rerr = Errf(CodeMethodNotFound, "宿主未实现 %s", req.Method)
			} else if !strings.HasPrefix(req.Method, "host.") {
				rerr = Errf(CodeMethodNotFound, "未知方法 %s", req.Method)
			} else {
				result, rerr = sh.handler(ctx, probe.Plugin, req.Method, req.Params)
			}
			resp := Response{JSONRPC: JSONRPCVersion, ID: req.ID, Plugin: probe.Plugin}
			if rerr != nil {
				resp.Error = rerr
			} else {
				b, err := json.Marshal(result)
				if err != nil {
					resp.Error = Errf(CodeInternalError, "序列化响应失败: %v", err)
				} else {
					resp.Result = b
				}
			}
			raw, err := json.Marshal(resp)
			if err != nil {
				sh.log.Warn("回包序列化失败", "err", err)
				return
			}
			if err := sh.writeRaw(raw); err != nil {
				sh.log.Warn("回包失败", "err", err)
			}
		}()
		return
	}

	// 响应：按 (plugin, id) 复合键路由
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return
	}
	key := probe.Plugin + "/" + RPCIDKey(probe.ID)
	sh.pendMu.Lock()
	ch := sh.pending[key]
	sh.pendMu.Unlock()
	if ch != nil {
		select {
		case ch <- &resp:
		default:
		}
	}
}

// Stop 关闭整个共享宿主（禁用 cohost / 管理器退出时）。
func (sh *SharedHost) Stop() {
	if !sh.closing.CompareAndSwap(false, true) {
		return
	}
	_ = sh.stdin.Close()
	go func() {
		select {
		case <-sh.done:
		case <-time.After(5 * time.Second):
			if sh.cmd != nil && sh.cmd.Process != nil {
				_ = sh.cmd.Process.Kill()
			}
		}
	}()
}

// ErrCohostFull 表示共享组已到 cohost_group_size 上限；
// startPlugin 收到它后回退独占进程（设计承诺的"组满回退"）。
var ErrCohostFull = errors.New("共享组已满")

// MemberCount 返回当前组员数（管理器用来限员）。
func (sh *SharedHost) MemberCount() int {
	sh.memberMu.Lock()
	defer sh.memberMu.Unlock()
	return len(sh.members)
}
