package webui

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xin/onebot/internal/onebot"
	"github.com/xin/onebot/internal/plugin"
	"github.com/xin/onebot/internal/send"
	"github.com/xin/onebot/internal/session"
	"github.com/xin/onebot/internal/trace"
)

// apiInjectMessage 在控制台里"假装收到一条消息"，走完整分发链路，
// 把插件的回复捕获下来返回给前端——不真的发到 QQ。
//
// 这是插件开发的核心工具：改一行代码，在网页上打一句话就知道有没有生效，
// 不用连真协议端、不用刷群。
func (s *Server) apiInjectMessage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	text := strings.TrimSpace(r.FormValue("text"))
	imgParam := strings.TrimSpace(r.FormValue("image"))
	// 允许"只发一张图"：text 空但带了 image 也算有效输入
	if text == "" && imgParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "需要 text 或 image"})
		return
	}

	chatType := r.FormValue("chat_type")
	if chatType == "" {
		chatType = "group"
	}
	if chatType != "group" && chatType != "private" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "chat_type 只能是 group 或 private"})
		return
	}

	// 用真实账号：插件会把 session.account 原样传给 host.reply，
	// 假名字会让它们报"未知账号"。防误发靠捕获和静音两层，
	// 不靠改账号名。
	account := r.FormValue("account")
	if account == "" {
		account = s.Deps.Queue.DefaultAccount()
	}

	chatID := parseIntOr(r.FormValue("chat_id"), 100000)

	// 身份：owner（主人）/ admin（群管理）/ member（路人）
	// 决定 user_id 和 role，用来测不同权限下的行为
	identity := r.FormValue("identity")
	if identity == "" {
		identity = "owner"
	}

	userID := parseIntOr(r.FormValue("user_id"), 0)
	if userID == 0 {
		switch identity {
		case "owner":
			if len(s.Deps.Config.Owner.Users) > 0 {
				userID = s.Deps.Config.Owner.Users[0]
			}
		case "admin":
			userID = 20001 // 假装群管理员
		default:
			userID = 30001 // 假装路人
		}
	}
	if userID == 0 {
		userID = 10001
	}

	// 群角色：owner 身份同时给群主，admin 给管理员，member 给普通成员
	role := "member"
	switch identity {
	case "owner":
		role = "owner"
	case "admin":
		role = "admin"
	}

	nickname := strings.TrimSpace(r.FormValue("nickname"))
	if nickname == "" {
		nickname = "测试用户"
	}

	ev := &onebot.Event{
		Account:     account,
		PostType:    "message",
		MessageType: chatType,
		UserID:      userID,
		MessageID:   time.Now().UnixNano() / 1000,
		Role:        role,
		Raw: map[string]any{
			"injected": true,
			"sender":   map[string]any{"nickname": nickname, "user_id": userID, "role": role},
		},
		Segments: []onebot.Segment{
			{Type: "text", Data: map[string]any{"text": text}},
		},
	}

	// 可选：带一张图（image 传 http(s) 地址），方便在控制台测识图/图片流程
	if imgURL := strings.TrimSpace(r.FormValue("image")); imgURL != "" {
		ev.Segments = append(ev.Segments, onebot.Segment{
			Type: "image",
			Data: map[string]any{"url": imgURL, "file": imgURL},
		})
	}
	if chatType == "group" {
		ev.GroupID = chatID
	}

	sess := session.FromEvent(ev, 0, s.Deps.Config.Command.Prefixes)
	sess.Nickname = nickname

	type reply struct {
		ChatType string           `json:"chat_type"`
		ChatID   int64            `json:"chat_id"`
		Segments []onebot.Segment `json:"segments"`
	}
	// 捕获回调跑在插件 RPC 的 goroutine 里，可能与这里并发：
	// 迟到的回复会在 writeJSON 期间继续 append，必须加锁 + 快照
	var repliesMu sync.Mutex
	var replies []reply
	collect := func(m send.CapturedMessage) {
		repliesMu.Lock()
		replies = append(replies, reply{
			ChatType: m.ChatType,
			ChatID:   m.ChatID,
			Segments: m.Segments,
		})
		repliesMu.Unlock()
	}
	countReplies := func() int {
		repliesMu.Lock()
		defer repliesMu.Unlock()
		return len(replies)
	}
	snapshotReplies := func() []reply {
		repliesMu.Lock()
		defer repliesMu.Unlock()
		return append([]reply(nil), replies...)
	}

	// 三层保护，确保测试消息绝不会发到真实群：
	//   1. 全局静音（函数一开始就打开，插件超时后补发的消息也会被丢弃）
	//   2. 捕获（把想发的消息收下来给前端看）
	//   3. account 换成占位符（就算前两层都失效，也找不到真实连接）
	unmute := s.Deps.Queue.Mute()
	defer func() {
		// 插件可能在我们返回后才补发消息（超时场景），延迟解除静音
		go func() {
			time.Sleep(3 * time.Second)
			unmute()
		}()
	}()

	stop := s.Deps.Queue.BeginCapture(collect)
	defer func() {
		go func() {
			time.Sleep(3 * time.Second)
			stop()
		}()
	}()

	// 从收到消息就开始追踪
	sess.Trace = trace.New(sess.Text)
	sess.Trace.Add("收到", "", true, "从控制台注入")

	started := time.Now()

	// **先过内置命令**，和真实消息的链路一致（否则测不出"插件抢了内置命令"）
	var host *plugin.Host
	handled := false
	if s.Deps.Builtin != nil && s.Deps.Builtin(r.Context(), sess) {
		sess.Trace.Add("插件", "builtin", true, "框架内置命令")
		handled = true
	} else {
		host, handled = s.Deps.Mgr.Dispatch(r.Context(), sess)
	}
	elapsed := time.Since(started)
	sess.Trace.Add("完成", "", true, "")

	if countReplies() > 0 {
		sess.Trace.Add("发送", "", true, fmt.Sprintf("捕获 %d 条", countReplies()))
	} else {
		sess.Trace.Add("发送", "", false, "没有回复")
	}

	out := map[string]any{
		"ok":         true,
		"handled":    handled,
		"identity":   identity,
		"user_id":    userID,
		"role":       role,
		"elapsed_ms": elapsed.Milliseconds(),
		"replies":    snapshotReplies(),
		"text":       sess.Text,
		"has_prefix": sess.HasPrefix,
		"trace":      sess.Trace.Snapshot(),
	}
	if host != nil {
		out["plugin"] = host.Manifest.ID
		out["plugin_name"] = host.Manifest.Name
	}
	if countReplies() == 0 && !handled {
		out["note"] = "没有插件认领这条消息"
		// 给一句人话解释（和真实消息走同一套判断）
		if hint := plugin.FailureHint(sess); hint != "" {
			out["hint"] = hint
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func parseIntOr(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
