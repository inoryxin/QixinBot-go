package main

// 装配与启动：配置、账号、内置命令、日志。

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xin/onebot/internal/audit"
	"github.com/xin/onebot/internal/backup"
	"github.com/xin/onebot/internal/chatbuf"
	"github.com/xin/onebot/internal/config"
	"github.com/xin/onebot/internal/help"
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
	"github.com/xin/onebot/internal/shared"
	"github.com/xin/onebot/internal/storage"
	"github.com/xin/onebot/internal/trace"
	"github.com/xin/onebot/internal/webui"
)

var version = "1.0.0"

func main() {
	configPath := flag.String("config", "config.toml", "配置文件路径")
	doBackup := flag.String("backup", "", "打包运行目录到指定 zip 后退出")
	doRestore := flag.String("restore", "", "从 zip 恢复后退出")
	force := flag.Bool("force", false, "恢复时允许覆盖已有数据")
	flag.Parse()

	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法确定工作目录:", err)
		os.Exit(1)
	}

	// 备份/恢复不需要加载配置，先处理
	if *doBackup != "" {
		// 框架可能正在运行（WAL 里有未合并的写入）：
		// 用独立连接把 WAL 收进主库再拷，避免备份出撕裂的 qixin.db
		checkpointSQLite(filepath.Join(root, "data", "qixin.db"))
		n, size, err := backup.Create(root, *doBackup)
		if err != nil {
			fmt.Fprintln(os.Stderr, "备份失败:", err)
			os.Exit(1)
		}
		fmt.Printf("已备份 %d 个文件，%.1f MB → %s\n", n, float64(size)/1024/1024, *doBackup)
		return
	}
	if *doRestore != "" {
		n, err := backup.Restore(root, *doRestore, *force)
		if err != nil {
			fmt.Fprintln(os.Stderr, "恢复失败:", err)
			os.Exit(1)
		}
		fmt.Printf("已恢复 %d 个文件。启动前请确认 config.toml 的 [web] 密码。\n", n)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		fmt.Fprintln(os.Stderr, "提示：复制 config.example.toml 为 config.toml 后修改 ws_url")
		os.Exit(1)
	}

	logger, logBuf, logLevel, logWriter := newLogger(cfg.Log)
	startedAt := time.Now()
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 协议端连接
	clients := make(map[string]*onebot.Client, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		clients[a.ID] = onebot.New(a.ID, a.WSURL, a.AccessToken, logger)
	}

	checker := perm.New(cfg.Permissions.Mode, cfg.Owner.Users)
	reg := registry.New()
	queue := send.New(ctx, clients, time.Duration(cfg.Send.IntervalMS)*time.Millisecond, logger)

	packagesDir := config.Abs(root, cfg.Plugin.Dir)
	runtimeDir := config.Abs(root, cfg.Plugin.RuntimeDir)
	stateDir := config.Abs(root, cfg.Plugin.StateDir)

	ov := override.New(filepath.Join(stateDir, "overrides.json"))
	auditLog := audit.New(filepath.Join(stateDir, "audit"))
	// 服务器资源采样（控制台概览页用）
	mon := monitor.New(stateDir)
	go mon.Run(ctx.Done(), 3*time.Second)

	// 持久化后端：默认 JSON，配了 sqlite 就用 SQLite（首次自动迁移）
	storagePath := cfg.Storage.StoragePath(root)
	var backend storage.Backend
	if cfg.Storage.DriverOrDefault() == "sqlite" {
		sq, err := storage.OpenSQLite(storagePath)
		if err != nil {
			logger.Error("打开 SQLite 失败，退回 JSON", "err", err, "path", storagePath)
			jb, jerr := storage.OpenJSON(filepath.Join(stateDir, "store.json"))
			if jerr != nil {
				logger.Error("打开 JSON 存储也失败", "err", jerr)
				return
			}
			backend = jb
		} else {
			backend = sq
			logger.Info("使用 SQLite 存储", "path", storagePath)
		}
	} else {
		jb, err := storage.OpenJSON(storagePath)
		if err != nil {
			logger.Error("打开 JSON 存储失败", "err", err)
			return
		}
		backend = jb
	}
	defer backend.Close()

	// 老版本的数据在 shared.json，第一次切 SQLite 时迁进来
	sharedStore := shared.NewWithBackend(backend, filepath.Join(stateDir, "shared.json"))
	chatLog := chatbuf.New(500, filepath.Join(stateDir, "chat.json"))

	// 先把 plugins/*.anzu 解压到运行区；新装的一律默认未启用
	// 可信公钥 = config 里配的 + 控制台里热加的（data/trusted-keys.json）
	trust := plugin.NewTrustStore(filepath.Join(stateDir, "trusted-keys.json"), cfg.Plugin.TrustedKeys, logger)
	inst := plugin.NewInstaller(packagesDir, runtimeDir, plugin.EffectiveKeys(trust.All()), cfg.Plugin.AllowUnsigned, logger)
	fresh, err := inst.Sync()
	if err != nil {
		logger.Error("插件安装失败", "err", err)
	}

	mgr := plugin.NewManager(
		runtimeDir,
		filepath.Join(stateDir, "plugins"),
		filepath.Join(stateDir, "plugins-state.json"),
		reg, checker, logger,
	)
	mgr.PackagesDir = packagesDir
	mgr.SharedDepsDir = filepath.Join(stateDir, "deps")
	mgr.RuntimeRoot = filepath.Join(stateDir, "runtimes")
	mgr.PruneIncompatibleRuntimes()
	// 上传安装也走安装器：签名校验 / 数据保留 / install.json 记账都在那边
	mgr.Inst = inst
	// 覆盖升级前先停进程：Windows 上插件占着目录，不停就没法换包
	inst.BeforeUpgrade = mgr.StopForUpgrade
	// 管理员在 WebUI 里改的优先级优先于清单
	mgr.PriorityOf = func(id string) (int, bool) { return ov.Priority(id) }

	// 账号顺序注入：DefaultAccount 的确定性兜底
	accIDs := make([]string, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		accIDs = append(accIDs, a.ID)
	}
	queue.SetAccountOrder(accIDs)

	// 共享宿主开关（默认开，cohost_enabled = false 显式关闭）
	mgr.SetCohostEnabled(cfg.Plugin.CohostEnabled.Bool())
	mgr.SetCohostGroupSize(cfg.Plugin.CohostGroupSize)
	if cfg.Plugin.CohostEnabled.Bool() {
		// 日志打生效值：未配置时 Manager 用默认 6，打 0 会误导
		gs := cfg.Plugin.CohostGroupSize
		if gs <= 0 {
			gs = plugin.CohostGroupSizeDefault
		}
		logger.Info("共享运行时宿主已开启", "group_size", gs)
	}

	// 运行指标：24h 时间序列（窗口滚动 + 内存/队列深度采样）
	mx := metrics.New(queue.Depth)
	queue.SetOnSent(mx.Sent)
	mgr.OnPluginCall = mx.PluginCall
	mx.Start(ctx)

	// 框架内置 db.sqlite 能力。
	//
	// 注意：插件用的是一个**独立的**数据库文件（data/plugins.db），
	// 和框架内部的 KV（data/qixin.db）分开。
	// 否则插件一句 SELECT * FROM kv 就能读别的插件的私有数据——
	// 进程隔离不等于数据隔离，这里必须物理分开。
	if _, ok := backend.(*storage.SQLite); ok {
		pluginDBPath := filepath.Join(stateDir, "plugins.db")
		pluginDB, err := storage.OpenPluginDB(pluginDBPath)
		if err != nil {
			logger.Error("打开插件数据库失败，db.sqlite 不可用", "err", err, "path", pluginDBPath)
		} else {
			mgr.RegisterBuiltins(pluginDB.DB())
			logger.Info("插件数据库已就绪", "path", pluginDBPath)
		}
	}
	mgr.Shared = sharedStore
	mgr.DebugGate.Store(cfg.Log.GateDebug)
	mgr.Env = cfg.Plugin.Env
	// 主人 QQ 注入所有插件进程（插件用它判断说话的人是不是主人，例如贴吧模式对主人免骂）。
	// 必须放在 mgr.Env 赋值之后：那行会整个替换环境变量表。
	if len(cfg.Owner.Users) > 0 {
		owners := make([]string, 0, len(cfg.Owner.Users))
		for _, u := range cfg.Owner.Users {
			owners = append(owners, strconv.FormatInt(u, 10))
		}
		mgr.SetEnv("QIXIN_OWNER", strings.Join(owners, "|"))
	}

	// 解释器路径：管理员可以在界面上指定（data/runtime.json），
	// 也让插件通过环境变量用同一个解释器。
	rtPath := filepath.Join(stateDir, "runtime.json")
	if raw, err := os.ReadFile(rtPath); err == nil {
		var rt map[string]string
		if json.Unmarshal(raw, &rt) == nil {
			plugin.SetRuntimeOverrides(rt)
			if v := rt["node"]; v != "" {
				mgr.SetEnv("QIXIN_NODE", v)
			}
			if v := rt["python"]; v != "" {
				mgr.SetEnv("QIXIN_PYTHON", v)
			}
		}
	}
	if cfg.Plugin.SDKDir != "" {
		mgr.SDKDir = cfg.Plugin.SDKDir
	} else {
		mgr.SDKDir = filepath.Join(root, "sdk", "node")
	}
	mgr.Overrides = ov
	store := newStore(runtimeDir)
	// 发送白名单热快照：host.reply 与 host.call_onebot 共用一份，
	// 配置热重载只换值，两边瞬间同步生效
	allow := &allowList{}
	allow.set(cfg.Send.AllowGroups)

	mgr.OnHostCall = hostCallHandler(hostEnv{
		cfg: cfg, queue: queue, clients: clients, store: store, mgr: mgr,
		auditLog: auditLog, dataRoot: stateDir, sharedStore: sharedStore,
		chatLog: chatLog, logger: logger, allow: allow,
	})

	for _, id := range fresh {
		if cfg.Plugin.AutoEnable {
			logger.Info("新插件已安装，自动启用（开发模式）", "plugin", id)
			continue
		}
		_ = mgr.Disable(id)
		logger.Info("新插件已安装，默认未启用", "plugin", id)
	}
	logger.Info("启动", "accounts", len(clients), "plugin_dir", mgr.Dir, "perm_mode", cfg.Permissions.Mode)
	// 配置热重载：5 秒轮询 config.toml，热字段（日志级别/门禁调试/
	// 发送限频/群白名单）改了立即生效，不用重启
	startHotReload(ctx, *configPath, logLevel, mgr, queue, allow, logger)

	mgr.Load(ctx)
	mgr.StartBackground(ctx)

	mgr.CheckUses()

	// 投放区热插拔：丢包进 plugins/ 或删包，不用重启
	if cfg.Plugin.HotReload {
		inst.AfterUpgrade = func(id string, err error) {
			mgr.AbortUpgrade(id)
			if err != nil {
				logger.Error("升级失败，已解除升级锁", "plugin", id, "err", err)
			}
		}
		go inst.WatchPackages(ctx, 2*time.Second, func() {
			// 先停掉包已删除的插件：Windows 下它们占着运行目录，不停就改不了名
			for _, id := range inst.MissingRuntimeIDs() {
				logger.Info("包已删除，先停插件", "plugin", id)
				mgr.StopForRemoval(id)
			}

			fresh, err := inst.Sync()
			if err != nil {
				// 部分包失败也要继续：已成功安装的插件必须重新对齐
				logger.Error("投放区同步失败（继续对齐已安装的）", "err", err)
			}
			logger.Info("投放区有变化，重新对齐插件", "new", fresh)
			mgr.ReloadFromDisk(ctx)
		})
	}

	mgr.CheckNames()
	warnBuiltinCollisions(reg, logger)
	logger.Info("插件状态", "summary", mgr.Describe())
	if cfg.Plugin.HotReload {
		go mgr.Watch(ctx)
	}

	// 控制台
	if cfg.Web.Enable {
		deps := &webui.Deps{
			StartedAt:  startedAt,
			Version:    version,
			Config:     cfg,
			ConfigPath: *configPath,
			Clients:    clients,
			RuntimeDir: runtimeDir,
			Mgr:        mgr,
			Checker:    checker,
			Reg:        reg,
			Overrides:  ov,
			Logs:       logBuf,
			Inst:       inst,
			Trust:      trust,
			// 让控制台注入也走内置命令，测试才和真实链路一致
			Builtin: func(ctx context.Context, sess *session.Session) bool {
				return handleBuiltin(ctx, sess, reg, checker, queue, mgr, ov, cfg.Command.Prefixes, logger)
			},
			Audit:   auditLog,
			Chat:    chatLog,
			Monitor: mon,
			Queue:   queue,
			Metrics: mx,
		}
		web := webui.New(cfg.Web.Addr, cfg.Web.Username, cfg.Web.Password, deps, logger)
		// 只有配置了可信代理才采信 X-Forwarded-For，否则限速能被伪造头绕过
		web.TrustedProxies = cfg.Web.TrustedProxies
		go func() {
			if err := web.Listen(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("控制台退出", "err", err)
			}
		}()
		defer func() {
			_ = web.Close(context.Background())
		}()
	}

	// 每个账号：维持连接 + 消费事件
	dedup := &deduper{seen: make(map[string]time.Time)}
	var wg sync.WaitGroup
	for id, c := range clients {
		wg.Add(2)
		go func(id string, c *onebot.Client) {
			defer wg.Done()
			c.Run(ctx)
		}(id, c)
		go func(id string, c *onebot.Client) {
			defer wg.Done()
			consume(ctx, c, cfg, mgr, reg, checker, queue, dedup, ov, auditLog, chatLog, mx, logger)
		}(id, c)
	}

	<-ctx.Done()
	logger.Info("正在关闭…")
	// 全局 30s 兜底：单个插件的停止内部本就有 5s 上限且并发执行，
	// 这里防的是"万一全体卡死"时关机流程无限挂着
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	mgr.StopAll(stopCtx) // 通知全部插件退出——Windows 上父进程退出不会带走子进程
	for _, c := range clients {
		c.Close()
	}
	// 等消息处理 goroutine 全部收尾后再落盘聊天记录：
	// Flush 放在 wg.Wait 之前的话，最后几条并发消息会漏掉
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		logger.Warn("消息处理未在 10 秒内收尾，强制退出")
	}
	chatLog.Flush()    // 别丢最后 1 秒的聊天记录（节流落盘）
	logger.Info("已退出") // 在关日志文件之前，最后一条要能进文件
	_ = logWriter.Close()
}

// consume 是消息入口。
//
// **它只负责收和投递，不处理业务**——这是关键。
// 之前是"收一条处理一条"，一个群的插件慢，所有群都排队。
//
// 现在的结构：
//
//	OneBot ──► consume（快速）
//	              ├─ 先查 ask（用户的回答不排队，直接投给等待者）
//	              └─ 按会话投递到各自的队列
//	                     ├─ 群A worker ──► handleOne（可并行）
//	                     ├─ 群B worker ──► handleOne
//	                     └─ 群C worker ──► handleOne
//
// 同一个群永远只有一个 worker（保序），不同群并行。

// consume 是消息入口。
//
// **它只负责收和投递，不处理业务**——这是关键。
// 之前是"收一条处理一条"，一个群的插件慢，所有群都排队。
//
// 现在的结构：
//
//	OneBot ──► consume（快速）
//	              ├─ 先查 ask（用户的回答不排队，直接投给等待者）
//	              └─ 按会话投递到各自的队列
//	                     ├─ 群A worker ──► handleOne（可并行）
//	                     ├─ 群B worker ──► handleOne
//	                     └─ 群C worker ──► handleOne
//
// 同一个群永远只有一个 worker（保序），不同群并行。
func consume(
	ctx context.Context,
	c *onebot.Client,
	cfg *config.Config,
	mgr *plugin.Manager,
	reg *registry.Registry,
	checker *perm.Checker,
	queue *send.Queue,
	dedup *deduper,
	ov *override.Store,
	auditLog *audit.Logger,
	chatLog *chatbuf.Buffer,
	mx *metrics.Collector,
	logger *slog.Logger,
) {
	handleOne := newHandler(ctx, cfg, mgr, reg, checker, queue, ov, auditLog, chatLog, logger)
	pool := newSessionPool(ctx, handleOne, logger)
	defer pool.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.Events():
			if ev.PostType != "message" {
				if ev.PostType == "notice" || ev.PostType == "request" {
					mgr.DispatchEvent(ctx, session.FromOneBotEvent(ev))
				}
				continue
			}
			if ev.MessageType != "group" && ev.MessageType != "private" {
				continue
			}
			if mx != nil {
				mx.MsgIn()
			}
			if ev.SelfID != 0 && ev.UserID == ev.SelfID {
				continue
			}
			key := fmt.Sprintf("%s:%d", ev.Account, ev.MessageID)
			if dedup.Seen(key) {
				logger.Debug("重复事件，已忽略", "key", key)
				continue
			}

			s := session.FromEvent(ev, c.SelfID(), cfg.Command.Prefixes)
			if s.Text == "" && len(s.Segments) == 0 {
				continue
			}
			s.Trace = trace.New(s.Text)

			// **关键**：交互等待中的回答要在**投递到队列之前**截走。
			// 否则它会被排在这个群的处理队列后面，
			// 而排在前面那条消息正在等它——互相等死。
			if mgr.TryAnswer(s) {
				logger.Debug("交互回答已投递", "user", s.UserID, "text", truncateText(s.Text, 40))
				continue
			}

			// 放在 TryAnswer 之后：回答交互等待的消息不需要引用内容，
			// 别让 get_msg 的往返（哪怕正常毫秒级）加进 ask 路径
			resolveReply(ctx, c, s, logger)

			pool.Submit(s, logger)
		}
	}
}

// resolveReply 尽力解析引用消息：带 [reply] 段时调协议端的 get_msg
// 换回被引用内容（谁发的、原文是什么）。AI 插件最常需要"他在回复哪条"。
//
// 失败（协议端不支持 get_msg / 消息已撤销 / 超时）只保留 ReplyID，
// 不打扰分发。3 秒上限：get_msg 正常是毫秒级，超 3 秒说明协议端有问题，
// 不能让它吃掉整个消息管道。

// resolveReply 尽力解析引用消息：带 [reply] 段时调协议端的 get_msg
// 换回被引用内容（谁发的、原文是什么）。AI 插件最常需要"他在回复哪条"。
//
// 失败（协议端不支持 get_msg / 消息已撤销 / 超时）只保留 ReplyID，
// 不打扰分发。3 秒上限：get_msg 正常是毫秒级，超 3 秒说明协议端有问题，
// 不能让它吃掉整个消息管道。
func resolveReply(ctx context.Context, c *onebot.Client, s *session.Session, logger *slog.Logger) {
	if s.ReplyID == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := c.Call(rctx, "get_msg", map[string]any{"message_id": s.ReplyID})
	if err != nil {
		logger.Debug("解析引用消息失败", "reply_id", s.ReplyID, "err", err)
		return
	}
	s.Reply = session.ParseReplyResponse(raw)
}

// newHandler 返回"处理单条消息"的闭包。

// newHandler 返回"处理单条消息"的闭包。
func newHandler(
	rootCtx context.Context,
	cfg *config.Config,
	mgr *plugin.Manager,
	reg *registry.Registry,
	checker *perm.Checker,
	queue *send.Queue,
	ov *override.Store,
	auditLog *audit.Logger,
	chatLog *chatbuf.Buffer,
	logger *slog.Logger,
) func(context.Context, *session.Session) {
	return func(ctx context.Context, s *session.Session) {
		scope := "private"
		if s.IsGroup() {
			scope = fmt.Sprintf("group %d", s.ChatID)
		}
		logger.Info("收到消息", "scope", scope, "user", s.UserID, "text", truncateText(s.RawText, 120))

		handlerCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Plugin.TimeoutMS)*time.Millisecond)
		defer cancel()

		// RawText 只含 text 段，纯图片/聊天记录的消息会是空的。
		inText := s.RawText
		if strings.TrimSpace(inText) == "" {
			inText = segmentsText(s.Segments)
		}

		inMsgID := chatLog.Add(chatbuf.Message{
			TS: s.TS, Direction: "in", Account: s.Account,
			ChatType: s.ChatType, ChatID: s.ChatID, UserID: s.UserID,
			Nickname: s.Nickname, Text: inText,
			Images: segmentImages(s.Segments, cfg.Plugin.StateDir),
		})

		// 内置命令优先，避免插件抢走 /help
		if handled := handleBuiltin(handlerCtx, s, reg, checker, queue, mgr, ov, cfg.Command.Prefixes, logger); handled {
			chatLog.Annotate(inMsgID, "builtin")
			return
		}

		dispatchStart := time.Now()
		host, handled := mgr.Dispatch(handlerCtx, s)
		if handled && host != nil {
			chatLog.Annotate(inMsgID, host.Manifest.ID)
			// **INFO 级别**：这是排查"这条消息谁处理的"的唯一线索，
			// 原来打 debug，info 级别的日志里根本看不到。
			logger.Info("消息已交给插件", "plugin", host.Manifest.ID,
				"scope", scope, "user", s.UserID, "ms", time.Since(dispatchStart).Milliseconds())
			auditLog.Write(audit.Entry{
				Account: s.Account, ChatType: s.ChatType, ChatID: s.ChatID, UserID: s.UserID,
				Plugin: host.Manifest.ID, Action: "command", Detail: s.Text, Allowed: true,
			})
			return
		}
		if !handled {
			fb := plugin.FailureHint(s)
			if fb != "" {
				_, _ = queue.SendText(handlerCtx, s.Account, s.ChatType, s.ChatID, fb, 0)
			}
		}

		// 带前缀但没人接：给个建议
		if s.HasPrefix {
			if sug, ok := help.Suggest(s.Text, commandNames(reg)); ok {
				msg := fmt.Sprintf("没有这个命令。你是不是想输入 %s%s ？", s.Prefix, sug)
				if _, err := queue.SendText(ctx, s.Account, s.ChatType, s.ChatID, msg, 0); err != nil {
					logger.Warn("发送建议失败", "err", err)
				}
			}
		}
	}
}

// sessionPool 是"按会话分队列 + 每会话一个 worker"的池子。
//
// 为什么要按会话分：
//   - 同一个群必须保序（连发两条指令不能乱）
//   - 不同群必须并行（一个群慢不该拖累别的）

func commandNames(reg *registry.Registry) []string {
	out := []string{"help", "plugins"}
	for _, f := range reg.All() {
		if f.Kind != registry.KindCommand {
			continue
		}
		name := strings.TrimPrefix(f.Usage, "/")
		if i := strings.IndexAny(name, " \t　"); i > 0 {
			name = name[:i]
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// handleBuiltin 处理框架自带命令，返回是否已处理。

// handleBuiltin 处理框架自带命令，返回是否已处理。
func handleBuiltin(ctx context.Context, s *session.Session, reg *registry.Registry, checker *perm.Checker, queue *send.Queue, mgr *plugin.Manager, ov *override.Store, prefixes []string, logger *slog.Logger) bool {
	if !s.HasPrefix {
		return false
	}
	fields := strings.Fields(s.Text)
	if len(fields) == 0 {
		return false
	}

	switch strings.ToLower(fields[0]) {
	case "help":
		text := ""
		if len(fields) >= 2 {
			arg := fields[1]
			// /help 2 → 按一级菜单的编号进二级
			if n, err := strconv.Atoi(arg); err == nil {
				if id, ok := help.PluginAt(reg, checker, s, ov, n); ok {
					arg = id
				}
			}
			if t, ok := help.RenderPlugin(reg, checker, s, arg, ov, prefixes); ok {
				text = t
			} else {
				text = fmt.Sprintf("没有找到插件：%s（/help 看编号）", fields[1])
			}
		} else {
			// 把"没有指令"的插件也点一下，免得群友以为它们不存在
			var idle []string
			for _, p := range mgr.List() {
				if p.Commands == 0 && p.State == "ready" {
					name := p.Name
					if name == "" {
						name = p.ID
					}
					idle = append(idle, name)
				}
			}
			text = help.Render(reg, checker, s, ov, prefixes, idle)
		}
		if _, err := queue.SendText(ctx, s.Account, s.ChatType, s.ChatID, text, 0); err != nil {
			logger.Error("发送 help 失败", "err", err)
		}
		return true

	// 内置叫 /plugins，不叫 /status：
	// /status 更自然的意思是「机器状态」（CPU/内存），那是插件干的事。
	case "plugins":
		if !checker.IsOwner(s.UserID) {
			return false
		}
		var b strings.Builder
		b.WriteString("插件状态\n")
		for _, st := range mgr.List() {
			fmt.Fprintf(&b, "%s v%s [%s]", st.ID, st.Version, st.State)
			if st.PID > 0 {
				fmt.Fprintf(&b, " pid=%d", st.PID)
			}
			b.WriteString("\n")
		}
		if _, err := queue.SendText(ctx, s.Account, s.ChatType, s.ChatID, b.String(), 0); err != nil {
			logger.Error("发送 status 失败", "err", err)
		}
		return true
	}
	return false
}

// newLogger 创建日志：stderr + 轮转文件（超 8MB 切分，留 4 份历史）。
// 返回 LevelVar 给配置热重载调级别（gate_debug 也走它）。
func newLogger(c config.Log) (*slog.Logger, *logbuf.Buffer, *slog.LevelVar, *sizeRotateWriter) {
	level := &slog.LevelVar{}
	level.Set(parseLevel(c.Level))

	var w io.Writer = os.Stderr
	var rw *sizeRotateWriter
	if c.File != "" {
		rw = newSizeRotateWriter(c.File)
		w = io.MultiWriter(os.Stderr, rw)
	}
	buf := logbuf.New(1000)
	return slog.New(logbuf.Wrap(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}), buf)), buf, level, rw
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// deduper 按 message_id 去重，避免重连后重复处理。

// deduper 按 message_id 去重，避免重连后重复处理。
type deduper struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (d *deduper) Seen(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if t, ok := d.seen[key]; ok && now.Sub(t) < 5*time.Minute {
		return true
	}
	d.seen[key] = now
	if len(d.seen) > 5000 {
		for k, t := range d.seen {
			if now.Sub(t) > 5*time.Minute {
				delete(d.seen, k)
			}
		}
	}
	return false
}

// asRPCError 保留原始错误码。
//
// 之前所有下游错误都被包成 CodeUnavailable，插件作者拿不到
// 文档承诺的 CodeRateLimited / CodeTimeout，没法区分"忙"和"坏"。

// builtinCommandNames 是框架自己处理的命令（不经插件）。
var builtinCommandNames = []string{"help", "plugins"}

// warnBuiltinCollisions 在启动时检查插件命令有没有和内置命令撞名。
//
// 为什么要检查：内置命令在 Dispatch **之前**执行，撞名的话插件那条命令
// 永远收不到消息（用户只会看到内置的回复），而且 /help 里两条都列着，
// 排查起来很费劲。撞名不阻止加载，但必须吵一句。

// warnBuiltinCollisions 在启动时检查插件命令有没有和内置命令撞名。
//
// 为什么要检查：内置命令在 Dispatch **之前**执行，撞名的话插件那条命令
// 永远收不到消息（用户只会看到内置的回复），而且 /help 里两条都列着，
// 排查起来很费劲。撞名不阻止加载，但必须吵一句。
func warnBuiltinCollisions(reg *registry.Registry, logger *slog.Logger) {
	builtin := map[string]bool{}
	for _, n := range builtinCommandNames {
		builtin[n] = true
	}
	seen := map[string]string{}
	for _, f := range reg.All() {
		if f.Kind != registry.KindCommand {
			continue
		}
		name := strings.TrimPrefix(f.Usage, "/")
		if i := strings.IndexAny(name, " \t　"); i > 0 {
			name = name[:i]
		}
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if builtin[lower] {
			logger.Warn("插件命令和内置命令撞名，插件这条永远不会触发",
				"command", name, "plugin", f.Plugin, "建议", "改插件 usage 或换个名字")
		}
		if prev, ok := seen[lower]; ok && prev != f.Plugin {
			logger.Warn("两个插件注册了同一个命令", "command", name, "a", prev, "b", f.Plugin)
		}
		seen[lower] = f.Plugin
	}
}

// segmentsSummary 把消息段压成审计里能看懂的一行。
// 例：你好呀 [图] [语音] [@123]
