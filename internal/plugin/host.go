package plugin

import (
	"bufio"
	"bytes"
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

// Handler 处理插件发来的 host.* 调用。
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, *RPCError)

// Host 管理一个插件子进程及其 stdio 上的 JSON-RPC 会话。
type Host struct {
	Manifest      *Manifest
	Dir           string         // 插件源码目录（进程工作目录）
	DataDir       string         // 插件私有数据目录（不受升级影响）
	SharedDepsDir string         // 共享依赖根目录（data/deps）
	Settings      map[string]any // 合并后的设置，握手时交给插件
	Log           *slog.Logger

	Handler Handler

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	writeMu sync.Mutex
	// stdinDead 表示写超时后 stdin 已被关闭，后续写直接失败，避免新旧帧交错
	stdinDead atomic.Bool
	// faulted 区分"主动停止"和"故障退出"：故障退出要触发 supervise 重启
	faulted atomic.Bool

	// Started 用于判断"稳定运行过"，久跑后再崩不该算连续崩溃。
	// Start 完成握手后写一次；读侧（管理器）可能并发，用原子时间戳。
	startedUnix atomic.Int64

	// hostReqWG 跟踪正在处理的宿主请求（关闭时等它们结束）。
	hostReqWG sync.WaitGroup
	hostReqMu sync.Mutex
	// hostReqSem 限制同时在处理的宿主请求数，防止插件狂发请求堆爆 goroutine。
	hostReqSem chan struct{}

	// callMu 保证同一插件的业务调用串行；ping/shutdown 走 Call 不受它影响。
	callMu sync.Mutex

	sessMu    sync.Mutex
	sessLocks map[string]*sync.Mutex

	pendMu  sync.Mutex
	pending map[string]chan *Response
	seq     atomic.Int64

	done       chan struct{}
	exitErr    error
	closing    atomic.Bool
	stderrMu   sync.Mutex
	stderrTail []string

	// ExtraEnv 是 [plugin] env 注入的额外环境变量（KEY=VALUE）
	ExtraEnv []string

	// cohost 非空 = 该插件跑在共享宿主里（不独占进程）。
	// Start/Call/Notify/Stop 等全部改走 SharedHost 的对应插件 worker；
	// 只在 [plugin] cohost_enabled 且 manifest 声明 cohost 时由 Manager 设置。
	cohost *SharedHost
}

// NewHost 创建宿主侧句柄，此时尚未启动进程。
func NewHost(m *Manifest, dir, dataDir string, log *slog.Logger) *Host {
	return &Host{
		Manifest: m,
		Dir:      dir,
		DataDir:  dataDir,
		Log:      log.With("plugin", m.ID),
		pending:  make(map[string]chan *Response),
		done:     make(chan struct{}),
		// 同时最多 32 个宿主请求在跑。host.ask 会长时间占一个名额，
		// 所以不能设太小（多群同时交互时会被打满）。
		hostReqSem: make(chan struct{}, 32),
	}
}

// checkExecutable 判断 exec[0] 能不能找到。
// 带路径分隔符的按文件判断，否则走 PATH；只对常见运行时给出安装提示。
func checkExecutable(name, dir string) error {
	if strings.ContainsAny(name, `/\`) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return nil
		}
		// Windows 上 exec.Command 能解析 "./deps/node" → "node.exe"，
		// 预检别比它更严：找不到就补一次常见扩展名再试
		for _, ext := range strings.Split(strings.TrimPrefix(os.Getenv("PATHEXT"), ";"), ";") {
			if ext == "" {
				continue
			}
			if st, err := os.Stat(path + strings.ToLower(ext)); err == nil && !st.IsDir() {
				return nil
			}
			if st, err := os.Stat(path + ext); err == nil && !st.IsDir() {
				return nil
			}
		}
		return fmt.Errorf("找不到可执行文件 %s（检查插件的 exec 和 deps/ 目录）", path)
	}

	if _, err := exec.LookPath(name); err == nil {
		return nil
	}

	switch name {
	case "node", "node.exe":
		return fmt.Errorf("PATH 里找不到 node。请安装 Node.js，或在 config.toml 的 [plugin.env] 里设置 PATH；" +
			"也可以把 node 放进插件包的 deps/ 并把 exec 改成 [\"./deps/node\", ...]")
	case "python", "python3", "python.exe":
		return fmt.Errorf("PATH 里找不到 python。请安装 Python 3，或在 config.toml 的 [plugin.env] 里设置 PATH")
	default:
		return fmt.Errorf("PATH 里找不到可执行文件 %q", name)
	}
}

// Start 拉起子进程并完成握手。
// 共享宿主模式：不 fork 进程，把插件 worker 加进共享组并完成握手。
func (h *Host) Start(ctx context.Context) error {
	if h.cohost != nil {
		return h.startCohost(ctx)
	}
	name := ResolveExecutable(h.Manifest.Exec[0])
	args := h.Manifest.Exec[1:]
	runtimeDir := ""
	if filepath.IsAbs(name) {
		runtimeDir = filepath.Dir(name)
	}

	// 先检查可执行文件在不在，给出比 fork/exec 更明确的提示
	if err := checkExecutable(name, h.Dir); err != nil {
		return fmt.Errorf("插件 %s 启动失败：%w", h.Manifest.ID, err)
	}

	cmd := exec.Command(name, args...)
	cmd.Dir = h.Dir

	// 注入插件自己的路径，省得插件算相对路径：
	//   QIXIN_PLUGIN_DIR  运行区根目录（app/ 的上一层）
	//   QIXIN_DATA_DIR    持久数据目录
	//   QIXIN_ASSETS_DIR  素材目录（app/assets 或 assets，哪个存在用哪个）
	env := os.Environ()
	env = append(env,
		"QIXIN_PLUGIN_ID="+h.Manifest.ID,
		"QIXIN_PLUGIN_DIR="+h.Dir,
		"QIXIN_DATA_DIR="+h.DataDir,
		"QIXIN_SHARED_DEPS_DIR="+h.SharedDepsDir,
		"QIXIN_RUNTIME_DIR="+runtimeDir,
	)
	for _, sub := range []string{"app/assets", "assets"} {
		candidate := filepath.Join(h.Dir, sub)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			env = append(env, "QIXIN_ASSETS_DIR="+candidate)
			break
		}
	}
	// 依赖目录：统一叫 deps/，按语言分子目录
	//   deps/node_modules  → NODE_PATH
	//   deps/python        → PYTHONPATH（pip install -t deps/python）
	//   deps/bin           → 加进 PATH（二进制工具）
	depsDir := filepath.Join(h.Dir, "deps")
	// 插件专属依赖优先，共享依赖兜底；两边都存在时都放进搜索路径。
	nodePaths := existingDirs(
		filepath.Join(depsDir, "node_modules"),
		filepath.Join(h.SharedDepsDir, "node", "node_modules"),
		filepath.Join(runtimeDir, "node_modules"),
	)
	if len(nodePaths) > 0 {
		env = append(env, "NODE_PATH="+strings.Join(nodePaths, string(os.PathListSeparator)))
	}
	pythonPaths := existingDirs(
		filepath.Join(depsDir, "python"),
		filepath.Join(h.SharedDepsDir, "python"),
		filepath.Join(runtimeDir, "python"),
	)
	if len(pythonPaths) > 0 {
		env = append(env, "PYTHONPATH="+strings.Join(pythonPaths, string(os.PathListSeparator)))
	}
	binPaths := existingDirs(
		filepath.Join(depsDir, "bin"),
		filepath.Join(h.SharedDepsDir, "bin"),
	)
	if len(binPaths) > 0 {
		// 追加到 PATH 末尾，不抢占系统命令
		env = append(env, "PATH="+os.Getenv("PATH")+string(os.PathListSeparator)+strings.Join(binPaths, string(os.PathListSeparator)))
	}

	// [plugin] env 覆盖放最后，允许用户手工改
	env = append(env, h.ExtraEnv...)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	h.cmd = cmd
	h.stdin = stdin
	h.stdout = stdout
	h.stderr = stderr

	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s: %w", name, err)
	}

	go h.readLoop()
	go h.stderrLoop()
	go func() {
		h.exitErr = cmd.Wait()
		close(h.done)
	}()

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := h.Call(initCtx, "plugin.initialize", map[string]any{
		"plugin_id":    h.Manifest.ID,
		"api_version":  "1.0",
		"data_dir":     h.DataDir,
		"host_version": "0.1.0",
		"settings":     h.Settings,
	}); err != nil {
		h.Kill()
		return fmt.Errorf("握手失败: %w", err)
	}

	pingCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	if _, err := h.Call(pingCtx, "plugin.ping", map[string]any{}); err != nil {
		h.Kill()
		return fmt.Errorf("ping 失败: %w", err)
	}

	h.Log.Info("插件已就绪", "version", h.Manifest.Version)
	h.markStarted()
	return nil
}

// markStarted 记录"进程已稳定就绪"的时间点，供稳定运行判定用。
func (h *Host) markStarted() { h.startedUnix.Store(time.Now().Unix()) }

// StartedSince 返回进程就绪至今的时长；还没就绪过返回 false。
func (h *Host) StartedSince() (time.Duration, bool) {
	ts := h.startedUnix.Load()
	if ts == 0 {
		return 0, false
	}
	return time.Since(time.Unix(ts, 0)), true
}

// startCohost 把 worker 加进共享组并完成握手。
// worker 崩溃后的自动重启 + 重初始化由 SharedHost 的 __worker_exit 逻辑负责；
// 这里的 Done 语义 = 共享进程退出（此时 supervise 会按插件逐个重启回组）。
func (h *Host) startCohost(ctx context.Context) error {
	entry := "app/index.js"
	if len(h.Manifest.Exec) > 1 {
		entry = h.Manifest.Exec[1]
	}
	settings := json.RawMessage("{}")
	if h.Settings != nil {
		if b, err := json.Marshal(h.Settings); err == nil {
			settings = b
		}
	}
	if err := h.cohost.EnsureMemberAndWait(ctx, CohostMember{
		ID:       h.Manifest.ID,
		Dir:      h.Dir,
		Entry:    entry,
		DataDir:  h.DataDir,
		Settings: settings,
	}); err != nil {
		// 初始化失败的 worker 还活着（空转占内存）：按指令停掉
		_ = h.cohost.StopPlugin(h.Manifest.ID)
		return fmt.Errorf("共享宿主就绪失败: %w", err)
	}
	// 共享进程退出 → 该插件 Done 关闭 → supervise 逐插件重启回组
	go func() {
		<-h.cohost.Done()
		select {
		case <-h.done:
		default:
			close(h.done)
		}
	}()
	h.Log.Info("插件已就绪（共享宿主）", "version", h.Manifest.Version)
	h.markStarted()
	return nil
}

// Call 发起一次请求并等待响应。
func (h *Host) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if h.cohost != nil {
		// 串行化语义与独占一致：Dispatcher 已通过 lockFor 拿了 h.callMu
		return h.cohost.Call(ctx, h.Manifest.ID, method, params)
	}
	id := "h-" + strconv.FormatInt(h.seq.Add(1), 10)
	ch := make(chan *Response, 1)

	h.pendMu.Lock()
	h.pending[id] = ch
	h.pendMu.Unlock()
	defer func() {
		h.pendMu.Lock()
		delete(h.pending, id)
		h.pendMu.Unlock()
	}()

	frame := Request{JSONRPC: JSONRPCVersion, ID: RPCStringID(id), Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		frame.Params = b
	}
	if err := h.write(frame); err != nil {
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
	case <-h.done:
		if h.exitErr != nil {
			return nil, fmt.Errorf("插件进程已退出: %w", h.exitErr)
		}
		return nil, errors.New("插件进程已退出")
	}
}

// Notify 发送 notification（不等待响应）。
func (h *Host) Notify(method string, params any) error {
	if h.cohost != nil {
		return h.cohost.NotifyPlugin(h.Manifest.ID, method, params)
	}
	frame := Request{JSONRPC: JSONRPCVersion, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		frame.Params = b
	}
	return h.write(frame)
}

// Ping 心跳。
func (h *Host) Ping(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := h.Call(c, "plugin.ping", map[string]any{})
	return err
}

// Stop 优雅关闭：shutdown → 关 stdin → 等待 → 必要时强杀。
// 共享宿主模式：只停本插件的 worker，不碰共享进程与其它插件。
func (h *Host) Stop(ctx context.Context) {
	if h.cohost != nil {
		if !h.beginClose() {
			return
		}
		_ = h.cohost.StopPlugin(h.Manifest.ID)
		return
	}
	if !h.beginClose() {
		return
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = h.Call(stopCtx, "plugin.shutdown", map[string]any{"reason": "host_shutdown"})

		if h.stdin != nil {
			_ = h.stdin.Close()
		}

		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			h.Log.Warn("插件未在 3 秒内退出，强制结束")
			h.Kill()
		}
	}()

	// 整体兜底：无论上面卡在哪，15 秒后一定返回，避免把上层调用方拖死
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		h.Log.Error("停止插件超时，强制结束", "plugin", h.Manifest.ID)
		h.Kill()
	}
}

// beginClose 在 hostReqMu 内设置关闭状态，和"检查-Add"互斥，
// 避免 Wait 在 Add 之前返回。
func (h *Host) beginClose() bool {
	h.hostReqMu.Lock()
	defer h.hostReqMu.Unlock()
	if h.closing.Load() {
		return false
	}
	h.closing.Store(true)
	return true
}

// Kill 强杀进程。共享宿主模式：worker 的终止走 StopPlugin。
func (h *Host) Kill() {
	if h.cohost != nil {
		h.Stop(context.Background())
		return
	}
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	// 在同一把锁内标记关闭：之后不会再有新的 Add
	h.beginClose()
	// 连插件起的子进程一起收掉（sidecar / worker 不能留孤儿）
	killProcessTree(h.cmd)

	// 等正在处理的宿主请求结束，但**必须有上限**：
	// Kill 常在持有 reloadMu 时被调用，而宿主请求可能正等着拿 reloadMu。
	// 无限等会直接死锁；超时就放行，让请求方自己失败退出。
	done := make(chan struct{})
	go func() {
		h.hostReqWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		h.Log.Warn("等待宿主请求结束超时，继续执行", "plugin", h.Manifest.ID)
	}
}

// Done 在进程退出时关闭。
func (h *Host) Done() <-chan struct{} { return h.done }

func (h *Host) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if h.stdinDead.Load() {
		return errors.New("stdin 已因写超时关闭，插件需要重启")
	}
	if h.stdin == nil {
		return errors.New("stdin 已关闭")
	}

	// 管道写可能永久阻塞（插件僵死或不读 stdin），必须带超时
	done := make(chan error, 1)
	go func() {
		_, werr := h.stdin.Write(b)
		done <- werr
	}()
	select {
	case werr := <-done:
		return werr
	case <-time.After(5 * time.Second):
		// 后台 Write 可能还在跑；不关掉它，下一帧会和它交错写坏协议。
		// 关 stdin 让 Write 立刻返回，并标记插件不可用；随后杀掉进程，
		// 让 supervise 走正常的重启流程，而不是卡在"活着但不能用"。
		h.stdinDead.Store(true)
		h.beginClose()        // 和 hostReqWG.Add 互斥，避免 Wait 提前返回
		h.faulted.Store(true) // 故障退出：supervise 必须重启
		_ = h.stdin.Close()
		go h.Kill()
		return errors.New("写 stdin 超时（插件可能已僵死），已终止并等待重启")
	}
}

// readLimitedLine 读一行（含结尾换行），但限制内容长度；
// 超过 max 时丢弃整行并返回 tooLong。
// 防止插件输出超长且不换行的一行，把宿主内存吃光。
// 注意边界：内容恰好等于 max 的合法行要能通过——
// 之前用 len(chunk) > max-len(buf) 判断，把恰好 max 的行当成超长丢掉了。
func readLimitedLine(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, e := r.ReadSlice('\n')
		// 内容长度按"不含换行"计；ErrBufferFull 表示还没看到换行，chunk 全是内容
		n := len(buf) + len(chunk)
		if e != bufio.ErrBufferFull && len(chunk) > 0 && chunk[len(chunk)-1] == '\n' {
			n--
		}
		if n > max {
			for e == bufio.ErrBufferFull {
				_, e = r.ReadSlice('\n')
			}
			return nil, true, nil
		}
		buf = append(buf, chunk...)
		if e == bufio.ErrBufferFull {
			continue
		}
		return buf, false, e
	}
}

func (h *Host) readLoop() {
	// 默认 64KB 的 Scanner 装不下 base64 图片，必须手动放大。
	r := bufio.NewReaderSize(h.stdout, 64<<10)
	for {
		// 多图消息的 base64 会拼进同一行：上限与消息总量兜底一致（64MB）
		line, tooLong, err := readLimitedLine(r, 64<<20)
		if tooLong {
			h.Log.Warn("插件输出单行过长，已丢弃", "plugin", h.Manifest.ID)
			continue
		}
		if len(bytes.TrimSpace(line)) > 0 {
			h.handleLine(bytes.TrimSpace(line))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				h.Log.Warn("读取插件输出失败", "err", err)
			}
			return
		}
	}
}

func (h *Host) handleLine(line []byte) {
	// id 用 RawMessage：JSON-RPC 2.0 允许数字 id，之前用 string 接，
	// 数字 id 解不出来，整行被当"stdout 污染"丢掉，插件就挂死等回包
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		h.Log.Warn("插件输出了非 JSON 行（stdout 被污染）", "line", truncate(line, 200))
		return
	}

	if probe.Method != "" {
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		// **并发处理宿主请求**，不能同步做。
		//
		// host.ask 会在 handleHostRequest 里等用户回复（可能几十秒）；
		// 如果这里同步等，readLoop 就卡住了——
		// 同一个插件的其他响应读不到、其他群的调用全部超时。
		//
		// 回包由 h.write 的写锁保证串行。
		//
		// **并发有上限**：插件短时间发大量 host.* 请求时，
		// 无限制的 goroutine 会把内存和宿主调用都堆满。
		// 超过上限的请求直接拒绝（返回错误而不是排队，
		// 因为排队的请求会让插件以为"发出去了"）。
		// **关闭检查和 Add 必须在同一把锁里**。
		// 否则 Kill 已经开始 Wait 之后，readLoop 仍可能 Add →
		// WaitGroup 语义被破坏（可能漏等，甚至 panic）。
		h.hostReqMu.Lock()
		if h.closing.Load() {
			h.hostReqMu.Unlock()
			return
		}
		sem := h.hostReqSem

		// **先拿名额，再起 goroutine**。
		// 反过来的话（先 go 再在 goroutine 里抢名额），
		// 突发请求仍然会创建大量 goroutine——它们只是立刻退出，
		// 但创建和调度的开销已经发生了。
		if sem != nil {
			select {
			case sem <- struct{}{}:
				// 拿到，继续
				default:
					h.hostReqMu.Unlock()
					h.Log.Warn("宿主请求过多，已拒绝", "method", req.Method, "limit", cap(sem))
					if len(req.ID) > 0 {
						_ = h.write(Response{
							JSONRPC: JSONRPCVersion, ID: req.ID,
							Error: Errf(CodeRateLimited, "宿主请求过多（上限 %d），请降低频率", cap(sem)),
						})
					}
					return
			}
		}

		// Add 在锁内（和 closing 检查原子），goroutine 在锁外起
		h.hostReqWG.Add(1)
		h.hostReqMu.Unlock()

		go func() {
			defer h.hostReqWG.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			h.handleHostRequest(&req)
		}()
		return
	}

	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return
	}
	h.pendMu.Lock()
	ch := h.pending[RPCIDKey(probe.ID)]
	h.pendMu.Unlock()
	if ch != nil {
		select {
		case ch <- &resp:
		default:
		}
	}
}

func (h *Host) handleHostRequest(req *Request) {
	// notification 没有 id，不需要回包。
	isNotify := len(req.ID) == 0

	// **这里不能用 init 超时**。
	// [timeouts] init 是"启动握手"的预算（默认 10 秒），
	// 而宿主调用可能是长时间的：host.ask 默认等 60 秒，
	// 能力调用可能配了 60 秒。用 10 秒截断会让它们全都提前失败。
	//
	// 真正的时间限制由各方法自己管：
	//   host.ask   → timeout_sec 参数
	//   capability → 调用方的 ctx
	// 这里只设一个"兜底上限"，防止某个方法挂死永远不返回。
	callTimeout := 30 * time.Minute
	if h.Manifest != nil {
		if d := h.Manifest.Timeouts.CallTimeout(); d > 0 {
			callTimeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	// 插件停用/崩溃时立刻取消，别让 host.ask 干等到 30 分钟兜底。
	// （ask 的等待在停用时已经清掉登记，这里再把 ctx 一起断掉。）
	go func() {
		select {
		case <-h.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	var result any = map[string]any{}
	var rerr *RPCError

	start := time.Now()
	if !strings.HasPrefix(req.Method, "host.") {
		rerr = Errf(CodeMethodNotFound, "未知方法 %s", req.Method)
	} else if h.Handler == nil {
		rerr = Errf(CodeMethodNotFound, "宿主未实现 %s", req.Method)
	} else {
		result, rerr = h.Handler(ctx, req.Method, req.Params)
	}
	// 慢调用诊断：超过 200ms 就记一条，方便定位卡在哪一步
	if spent := time.Since(start); spent > 200*time.Millisecond {
		h.Log.Warn("宿主调用慢", "method", req.Method, "ms", spent.Milliseconds(), "notify", isNotify)
	}

	if isNotify {
		return
	}

	if rerr != nil {
		h.Log.Debug("拒绝插件调用", "method", req.Method, "code", rerr.Code, "msg", rerr.Message)
	}
	resp := Response{JSONRPC: JSONRPCVersion, ID: req.ID}
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
	if err := h.write(resp); err != nil {
		h.Log.Warn("回包失败", "err", err)
	}
}

func (h *Host) stderrLoop() {
	r := bufio.NewReaderSize(h.stderr, 16<<10)
	for {
		line, tooLong, err := readLimitedLine(r, 256<<10)
		if tooLong {
			h.Log.Warn("插件错误输出单行过长，已丢弃", "plugin", h.Manifest.ID)
			continue
		}
		if s := strings.TrimSpace(string(line)); s != "" {
			h.Log.Info(s)
			h.stderrMu.Lock()
			h.stderrTail = append(h.stderrTail, s)
			if len(h.stderrTail) > 20 {
				h.stderrTail = h.stderrTail[len(h.stderrTail)-20:]
			}
			h.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}




// callAndRecord 执行一次任务并记录结果。

// LockSession 按会话加锁，返回解锁函数。
// 同一会话串行（保证战斗状态不乱），不同会话并发（10 个玩家不用排队）。
func (h *Host) LockSession(key string) func() {
	h.sessMu.Lock()
	if h.sessLocks == nil {
		h.sessLocks = make(map[string]*sync.Mutex)
	}
	l := h.sessLocks[key]
	if l == nil {
		l = &sync.Mutex{}
		h.sessLocks[key] = l
	}
	h.sessMu.Unlock()

	l.Lock()
	return l.Unlock
}

// ExitSummary 返回进程退出时的诊断信息：错误、退出码、最后几行 stderr。
func (h *Host) ExitSummary() string {
	h.stderrMu.Lock()
	tail := strings.Join(h.stderrTail, " | ")
	h.stderrMu.Unlock()

	code := "unknown"
	if h.cmd != nil && h.cmd.ProcessState != nil {
		code = fmt.Sprintf("%d", h.cmd.ProcessState.ExitCode())
	}
	msg := fmt.Sprintf("exit=%s", code)
	if h.exitErr != nil {
		msg += fmt.Sprintf(" err=%v", h.exitErr)
	}
	if tail != "" {
		msg += " stderr=" + truncate([]byte(tail), 400)
	}
	return msg
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// existingDirs 返回存在且是目录的路径，保持传入顺序。
func existingDirs(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}
