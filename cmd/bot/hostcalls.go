package main

// 插件 host.* 调用处理：OneBot 代理（分级+白名单）、能力、存储等。
//
// 分发是一张方法表（methods）：新增 host.* 方法 = 写一个 env 方法 +
// 表里注册一行，不用再碰分发循环。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/xin/onebot/internal/audit"
	"github.com/xin/onebot/internal/chatbuf"
	"github.com/xin/onebot/internal/config"
	"github.com/xin/onebot/internal/onebot"
	"github.com/xin/onebot/internal/plugin"
	"github.com/xin/onebot/internal/send"
	"github.com/xin/onebot/internal/session"
	"github.com/xin/onebot/internal/shared"
)

type chatParam struct {
	Account  string `json:"account"`
	ChatType string `json:"chat_type"`
	ChatID   int64  `json:"chat_id"`
}

// hostEnv 是 host.* 处理器的全部依赖，装配一次不再变
// （可变配置走 allowList / Queue 的原子字段，热重载只换值不换对象）。
type hostEnv struct {
	cfg         *config.Config
	queue       *send.Queue
	clients     map[string]*onebot.Client
	store       *store
	mgr         *plugin.Manager
	auditLog    *audit.Logger
	dataRoot    string
	sharedStore *shared.Store
	chatLog     *chatbuf.Buffer
	logger      *slog.Logger
	allow       *allowList // send.allow_groups 热快照
}

// hostMethodFunc 是单个 host.* 方法的统一签名。method 原样传入
// （一个函数可以同时注册给多个名字，比如 job_begin/job_end）。
type hostMethodFunc func(ctx context.Context, m *plugin.Manifest, method string, params json.RawMessage) (any, *plugin.RPCError)

func hostCallHandler(env hostEnv) plugin.HostCallFunc {
	handlers := env.methods()
	return func(ctx context.Context, m *plugin.Manifest, method string, params json.RawMessage) (any, *plugin.RPCError) {
		if h, ok := handlers[method]; ok {
			return h(ctx, m, method, params)
		}
		return nil, plugin.Errf(plugin.CodeMethodNotFound, "未知方法 %s", method)
	}
}

// methods 构建方法表。
func (env *hostEnv) methods() map[string]hostMethodFunc {
	add := func(mm map[string]hostMethodFunc, names []string, fn hostMethodFunc) {
		for _, n := range names {
			mm[n] = fn
		}
	}
	m := make(map[string]hostMethodFunc, 16)
	add(m, []string{"host.log"}, env.hostLog)
	add(m, []string{"host.job_begin", "host.job_end"}, env.hostJob)
	add(m, []string{"host.ask"}, env.hostAsk)
	add(m, []string{"host.reply", "host.send_message"}, env.hostReply)
	add(m, []string{"host.call_onebot"}, env.hostCallOnebot)
	add(m, []string{"host.storage_get"}, env.storageGet)
	add(m, []string{"host.storage_set"}, env.storageSet)
	add(m, []string{"host.storage_delete"}, env.storageDelete)
	add(m, []string{"host.storage_list"}, env.storageList)
	add(m, []string{"host.shared_list"}, env.sharedList)
	add(m, []string{"host.shared_get"}, env.sharedGet)
	add(m, []string{"host.shared_set"}, env.sharedSet)
	add(m, []string{"host.call_tool"}, env.callTool)
	add(m, []string{"host.list_tools"}, env.listTools)
	add(m, []string{"host.check_permission"}, env.checkPermission)
	add(m, []string{"host.list_access"}, env.listAccess)
	add(m, []string{"host.call_capability"}, env.callCapability)
	return m
}

// doSend 是所有"插件往外发消息"的唯一出口。
//
// host.reply / host.send_message / host.ask 都走这里，保证三件事一致：
//  1. 群发送白名单（send.allow_groups）
//  2. 审计记录（audit）
//  3. 聊天记录（chatbuf，捕获模式下跳过）
func (env *hostEnv) doSend(ctx context.Context, m *plugin.Manifest, chat chatParam, segs []onebot.Segment, replyTo int64) (int64, *plugin.RPCError) {
	if chat.Account == "" {
		chat.Account = env.queue.DefaultAccount()
		if chat.Account == "" {
			return 0, plugin.Errf(plugin.CodeUnavailable, "还没有已连接的账号")
		}
	}
	if chat.ChatType != "group" && chat.ChatType != "private" {
		return 0, plugin.Errf(plugin.CodeInvalidParams, "chat_type 只能是 group 或 private")
	}
	if len(segs) == 0 {
		return 0, plugin.Errf(plugin.CodeInvalidParams, "segments 为空")
	}
	if n := approxSize(segs); n > 7<<20 {
		return 0, plugin.Errf(plugin.CodeInvalidParams,
			"消息太大（约 %d MB）。图片请先存到 data/ 下再用文件路径，别直接塞 base64", n>>20)
	}

	// 发送白名单：配置了 allow_groups 就只允许发这些群（空 = 不限）
	if chat.ChatType == "group" && !env.allow.allowed(chat.ChatID) {
		env.auditLog.Write(audit.Entry{
			Account: chat.Account, ChatType: chat.ChatType, ChatID: chat.ChatID,
			Plugin: m.ID, Action: "send", Allowed: false, Reason: "群不在发送白名单",
			Detail: segmentsSummary(segs),
		})
		return 0, plugin.Errf(plugin.CodePermissionDenied,
			"插件 %s 无权向群 %d 发送消息（不在 send.allow_groups 里）", m.ID, chat.ChatID)
	}

	id, err := env.queue.Send(ctx, chat.Account, chat.ChatType, chat.ChatID, segs, replyTo)
	if err != nil {
		return 0, plugin.Errf(plugin.CodeInternalError, "发送失败: %v", err)
	}
	// 捕获模式（WebUI 测试注入）下消息没真发，不记聊天记录
	if !env.queue.IsCapturing() {
		env.chatLog.Add(chatbuf.Message{
			TS: time.Now().Unix(), Direction: "out", Account: chat.Account,
			ChatType: chat.ChatType, ChatID: chat.ChatID,
			Plugin: m.ID, Text: segmentsText(segs),
			Images: segmentImages(segs, env.dataRoot),
		})
	}
	detail := segmentsSummary(segs)
	if env.queue.IsCapturing() {
		detail = "[控制台注入] " + detail
	}
	env.auditLog.Write(audit.Entry{
		Account: chat.Account, ChatType: chat.ChatType, ChatID: chat.ChatID,
		Plugin: m.ID, Action: "send", Allowed: true,
		Detail: detail,
	})
	return id, nil
}

func (env *hostEnv) hostLog(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	_ = json.Unmarshal(params, &p)

	// **尊重插件传的级别**：之前一律记成 INFO，插件打的 debug
	// 也当 info 写进文件；空 fields 也不该输出（每行挂个
	// `fields=map[]` 全是噪音）。
	lvl := slog.LevelInfo
	switch strings.ToLower(p.Level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	attrs := []any{"plugin", m.ID}
	if len(p.Fields) > 0 {
		attrs = append(attrs, "fields", p.Fields)
	}
	env.logger.Log(context.Background(), lvl, p.Message, attrs...)
	return nil, nil
}

func (env *hostEnv) hostJob(_ context.Context, m *plugin.Manifest, method string, params json.RawMessage) (any, *plugin.RPCError) {
	// 后台任务登记（host.background 用；只是记账，不阻塞）
	var p struct {
		ID   string    `json:"id"`
		Name string    `json:"name"`
		Chat chatParam `json:"chat"`
		Err  string    `json:"error"`
	}
	_ = json.Unmarshal(params, &p)
	if method == "host.job_begin" {
		env.mgr.BeginJob(m.ID, p.ID, p.Name, p.Chat.ChatType, p.Chat.ChatID)
	} else {
		var jobErr error
		if p.Err != "" {
			jobErr = fmt.Errorf("%s", p.Err)
		}
		env.mgr.EndJob(m.ID, p.ID, jobErr)
	}
	return map[string]any{"ok": true}, nil
}

func (env *hostEnv) hostAsk(ctx context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	// 发一句提示，等用户下一条消息（超时返回空串，不报错）
	var p struct {
		Text    string    `json:"text"`
		Chat    chatParam `json:"chat"`
		Timeout int       `json:"timeout_sec"`
		UserID  int64     `json:"user_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "参数解析失败: %v", err)
	}
	if p.Chat.Account == "" {
		p.Chat.Account = env.queue.DefaultAccount()
	}

	timeout := time.Duration(p.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	// **先登记等待，再发提示**。
	// 反过来做的话，用户在提示刚发出、等待还没登记的空隙里秒回，
	// 那条消息会走正常分发，等待者永远收不到，最后超时。
	ch, cancelled, cancelWait := env.mgr.RegisterAsk(m.ID, p.Chat.Account, p.Chat.ChatType, p.Chat.ChatID, p.UserID)
	defer cancelWait()

	if p.Text != "" {
		// 提示语也走统一发送出口：白名单、审计、聊天记录一个都不能漏
		if _, rerr := env.doSend(ctx, m, p.Chat, []onebot.Segment{onebot.Text(p.Text)}, 0); rerr != nil {
			return nil, rerr
		}
	}

	select {
	case answer := <-ch:
		return map[string]any{"text": answer, "answered": answer != ""}, nil
	case <-cancelled:
		// 用户取消 / 被新等待顶替 / 插件卸载：立刻返回，不挂到超时
		return map[string]any{"text": "", "answered": false}, nil
	case <-time.After(timeout):
		return map[string]any{"text": "", "answered": false}, nil
	case <-ctx.Done():
		return nil, plugin.Errf(plugin.CodeTimeout, "等待超时或连接断开")
	}
}

func (env *hostEnv) hostReply(ctx context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Chat     chatParam       `json:"chat"`
		Segments json.RawMessage `json:"segments"`
		ReplyTo  int64           `json:"reply_to"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "参数解析失败: %v", err)
	}
	segs := parseSegments(p.Segments)
	if env.cfg.Command.Markdown != "off" {
		segs = renderMarkdown(segs)
	}
	segs, err := resolveMedia(segs, env.dataRoot)
	if err != nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "%v", err)
	}
	id, rerr := env.doSend(ctx, m, p.Chat, segs, p.ReplyTo)
	if rerr != nil {
		return nil, rerr
	}
	return map[string]any{"message_id": id}, nil
}

func (env *hostEnv) hostCallOnebot(ctx context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Action string         `json:"action"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "参数解析失败: %v", err)
	}
	level := actionLevel(p.Action)
	if !allowAction(m, level) {
		env.auditLog.Write(audit.Entry{
			Plugin: m.ID, Action: "onebot", Detail: p.Action, Allowed: false,
			Reason: "缺少 " + level + " 级别声明",
		})
		return nil, plugin.Errf(plugin.CodePermissionDenied,
			"插件 %s 无权调用 %s（级别 %s，需要在 manifest.toml 的 permissions 里声明）", m.ID, p.Action, level)
	}
	if level == "admin" || level == "dangerous" {
		env.auditLog.Write(audit.Entry{
			Plugin: m.ID, Action: "onebot", Detail: p.Action, Allowed: true, Reason: level,
		})
	}
	// 发消息类 action 与 host.reply 同一条规矩：只许发往 allow_groups
	// 白名单里的群（否则 onebot.write 插件用 send_group_msg 就绕开了
	// doSend 的全部出口管控）
	if strings.HasPrefix(p.Action, "send_") || p.Action == "send_msg" {
		if gid := groupIDOfParams(p.Params); gid != 0 && !env.allow.allowed(gid) {
			env.auditLog.Write(audit.Entry{
				Plugin: m.ID, Action: "onebot", Detail: p.Action, Allowed: false,
				Reason: fmt.Sprintf("群 %d 不在 send.allow_groups", gid),
			})
			return nil, plugin.Errf(plugin.CodePermissionDenied,
				"群 %d 不在 send.allow_groups 白名单里（用 host.reply 发消息则无此限制）", gid)
		}
	}
	account := firstAccount(env.cfg)
	client := env.clients[account]
	if client == nil {
		return nil, plugin.Errf(plugin.CodeUnavailable, "没有可用账号")
	}
	data, err := client.Call(ctx, p.Action, p.Params)
	if err != nil {
		var apiErr *onebot.APIError
		if errors.As(err, &apiErr) {
			return nil, plugin.Errf(plugin.CodeInternalError, "%s", apiErr.Error())
		}
		return nil, plugin.Errf(plugin.CodeInternalError, "%v", err)
	}
	return map[string]any{"data": json.RawMessage(data)}, nil
}

func (env *hostEnv) storageGet(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(params, &p)
	v, ok := env.store.Get(m.ID, p.Key)
	if !ok {
		return map[string]any{"value": nil}, nil
	}
	return map[string]any{"value": v}, nil
}

func (env *hostEnv) storageSet(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}
	_ = json.Unmarshal(params, &p)
	if err := env.store.Set(m.ID, p.Key, p.Value); err != nil {
		return nil, plugin.Errf(plugin.CodeInternalError, "%v", err)
	}
	return map[string]any{}, nil
}

func (env *hostEnv) storageDelete(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(params, &p)
	_ = env.store.Delete(m.ID, p.Key)
	return map[string]any{}, nil
}

func (env *hostEnv) storageList(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Prefix string `json:"prefix"`
	}
	_ = json.Unmarshal(params, &p)
	return map[string]any{"keys": env.store.List(m.ID, p.Prefix)}, nil
}

func (env *hostEnv) sharedList(_ context.Context, m *plugin.Manifest, _ string, _ json.RawMessage) (any, *plugin.RPCError) {
	return map[string]any{"items": env.sharedStore.List()}, nil
}

func (env *hostEnv) sharedGet(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(params, &p)
	v, ok := env.sharedStore.Get(p.Key)
	return map[string]any{"found": ok, "value": v}, nil
}

func (env *hostEnv) sharedSet(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}
	_ = json.Unmarshal(params, &p)
	if !env.sharedStore.Set(m.ID, p.Key, p.Value) {
		return nil, plugin.Errf(plugin.CodePermissionDenied,
			"只能写自己在 manifest 的 [[shared]] 里声明过的 key（%s 不能写 %s）", m.ID, p.Key)
	}
	return map[string]any{"ok": true}, nil
}

func (env *hostEnv) callTool(ctx context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
		// session 字段保留兼容但**不使用**：会话由框架绑定
		Session *session.Session `json:"session"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "参数解析失败: %v", err)
	}
	if p.Name == "" {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "需要 name")
	}
	// 不信任插件提交的身份：只用它带回来的令牌换可信会话
	token := ""
	if p.Session != nil {
		token = p.Session.CallToken
	}
	boundSess := env.mgr.SessionForToken(token)
	data, err := env.mgr.CallTool(ctx, p.Name, p.Args, boundSess)
	if err != nil {
		return nil, asRPCError(err)
	}
	return map[string]any{"result": data}, nil
}

func (env *hostEnv) listTools(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Session *session.Session `json:"session"`
	}
	_ = json.Unmarshal(params, &p)
	return map[string]any{"tools": env.mgr.ListToolsFor(p.Session)}, nil
}

func (env *hostEnv) checkPermission(_ context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Plugin  string           `json:"plugin"`
		Session *session.Session `json:"session"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "参数解析失败: %v", err)
	}
	if p.Plugin == "" || p.Session == nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "需要 plugin 和 session")
	}
	return map[string]any{"allowed": env.mgr.CheckAccess(p.Plugin, p.Session)}, nil
}

func (env *hostEnv) listAccess(_ context.Context, m *plugin.Manifest, _ string, _ json.RawMessage) (any, *plugin.RPCError) {
	return map[string]any{"access": env.mgr.AllAccess()}, nil
}

func (env *hostEnv) callCapability(ctx context.Context, m *plugin.Manifest, _ string, params json.RawMessage) (any, *plugin.RPCError) {
	var p struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, plugin.Errf(plugin.CodeInvalidParams, "参数解析失败: %v", err)
	}
	data, err := env.mgr.CallCapability(ctx, p.Name, p.Args, nil)
	if err != nil {
		return nil, asRPCError(err)
	}
	return map[string]any{"result": json.RawMessage(data)}, nil
}

// groupIDOfParams 从 send 类 action 的 params 里抠群号。
// params 刚从 JSON 解出来（数字=float64，有的实现发字符串），都接。
func groupIDOfParams(params map[string]any) int64 {
	if params == nil {
		return 0
	}
	switch v := params["group_id"].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return 0
}

// actionLevel 给 OneBot action 分级。未知 action 按 write 处理（保守）。
func actionLevel(action string) string {
	switch action {
	case "get_login_info", "get_status", "get_version_info", "get_group_list",
		"get_group_info", "get_group_member_list", "get_group_member_info",
		"get_friend_list", "get_stranger_info", "get_msg", "can_send_image",
		"can_send_record", "get_group_msg_history", "get_friend_msg_history":
		return "read"
	case "send_group_msg", "send_private_msg", "send_msg", "delete_msg",
		"set_group_card", "upload_group_file", "upload_private_file",
		"set_friend_add_request", "set_group_add_request", "mark_msg_as_read",
		"send_poke", "group_poke", "friend_poke":
		return "write"
	case "set_group_kick", "set_group_kick_members", "set_group_ban",
		"set_group_admin", "set_group_add_option",
		"set_essence_msg", "delete_essence_msg", "send_like":
		return "admin"
	case "get_cookies", "_send_group_notice", "send_qzone_msg", "delete_qzone_msg",
		"like_qzone", "upload_image_to_qun_album":
		return "dangerous"
	default:
		// 没归类的 action 不能当普通写操作：新增的管理/高危接口
		// 会因此被 onebot.write 直接调用。默认按 admin，显式分级才放行。
		return "admin"
	}
}

func allowAction(m *plugin.Manifest, level string) bool {
	has := func(p string) bool {
		for _, x := range m.Permissions {
			if x == p {
				return true
			}
		}
		return false
	}
	switch level {
	case "read":
		return true
	case "write":
		return has("onebot.write")
	case "admin":
		return has("onebot.admin")
	default:
		return has("onebot.dangerous")
	}
}

func firstAccount(cfg *config.Config) string {
	if len(cfg.Accounts) == 0 {
		return ""
	}
	return cfg.Accounts[0].ID
}

// asRPCError 保留原始错误码。
//
// 之前所有下游错误都被包成 CodeUnavailable，插件作者拿不到
// 文档承诺的 CodeRateLimited / CodeTimeout，没法区分"忙"和"坏"。
func asRPCError(err error) *plugin.RPCError {
	var rpcErr *plugin.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	return plugin.Errf(plugin.CodeUnavailable, "%v", err)
}
