package session

import (
	"encoding/json"
	"strings"

	"github.com/xin/onebot/internal/trace"

	"github.com/xin/onebot/internal/onebot"
)

// Session 是交给插件的一条消息上下文。字段名即协议名（snake_case），
// 直接序列化后交给插件，不要改这里的 json tag。
type Session struct {
	Account   string `json:"account"`
	ChatType  string `json:"chat_type"` // group | private
	ChatID    int64  `json:"chat_id"`
	UserID    int64  `json:"user_id"`
	MessageID int64  `json:"message_id"`
	SelfID    int64  `json:"self_id"`
	Nickname  string `json:"nickname"`
	Role      string `json:"role"`

	Text      string `json:"text"` // 已去掉命令前缀
	RawText   string `json:"raw_text"`
	HasPrefix bool   `json:"has_prefix"`
	Prefix    string `json:"prefix"`
	Mentioned bool   `json:"mentioned"`

	Segments []onebot.Segment `json:"segments"`
	TS       int64            `json:"ts"`

	// Trace 记录这条消息的处理路径（不发给插件，只给控制台看）。
	Trace *trace.Trace `json:"-"`

	// CallToken 是框架为这次分发生成的一次性令牌：
	// 插件把它原样带回来，宿主据此找到可信会话，而不是相信插件填的身份。
	CallToken string `json:"call_token,omitempty"`

	// Command 是宿主匹配到的结构化命令（manifest 里声明了 args 才有）。
	// 没声明 args 的插件照旧自己解析 text。
	Command *CommandMatch `json:"command,omitempty"`

	// 引用/回复上下文：消息带 [reply] 段时填充。
	// ReplyID 从消息段直接解析（一定有）；Reply 是宿主用 get_msg 尽力
	// 换回的内容——协议端不支持、消息已撤销或超时就是 nil，别假设它非空。
	ReplyID int64         `json:"reply_id,omitempty"`
	Reply   *ReplyContext `json:"reply,omitempty"`
}

// ReplyContext 是被引用消息的内容。
type ReplyContext struct {
	UserID   int64            `json:"user_id,omitempty"`
	Nickname string           `json:"nickname,omitempty"`
	Text     string           `json:"text,omitempty"`     // 段里的纯文本拼接
	Segments []onebot.Segment `json:"segments,omitempty"` // 原始段（图/表情等都在）
}

// ParseReplyResponse 解析协议端 get_msg 的返回，构造引用上下文。
// message 字段可能是段数组，也可能是 CQ 码字符串——两种都接。
// 解析失败返回 nil（调用方只保留 ReplyID）。
func ParseReplyResponse(raw json.RawMessage) *ReplyContext {
	var r struct {
		// sender 用 map 接：有的实现把 user_id 发成字符串，
		// 结构体直解会让整条引用内容跟着解析失败（toInt64 两种都认）
		Sender  map[string]any  `json:"sender"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil
	}
	nickname, _ := r.Sender["nickname"].(string)
	uid := toInt64(r.Sender["user_id"])
	// data:null（有的实现用 retcode=0 + null data 表示失败）会静默得到
	// 全零结构：什么都解析不出来就返回 nil，别让插件拿到 truthy 的空对象
	if uid == 0 && nickname == "" && len(r.Message) == 0 {
		return nil
	}
	rc := &ReplyContext{UserID: uid, Nickname: nickname}
	var segs []onebot.Segment
	if len(r.Message) > 0 && json.Unmarshal(r.Message, &segs) == nil && len(segs) > 0 {
		rc.Segments = segs
		rc.Text = segmentsText(segs)
		return rc
	}
	var cq string
	if len(r.Message) > 0 && json.Unmarshal(r.Message, &cq) == nil && cq != "" {
		rc.Segments = onebot.ParseMessage(cq)
		rc.Text = segmentsText(rc.Segments)
	}
	return rc
}

// segmentsText 拼出段里的纯文本。
func segmentsText(segs []onebot.Segment) string {
	var b strings.Builder
	for _, seg := range segs {
		if seg.Type != "text" {
			continue
		}
		if t, ok := seg.Data["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return strings.TrimSpace(b.String())
}

// IsGroup 判断是否群聊。
func (s *Session) IsGroup() bool { return s.ChatType == "group" }

// ChatKey 用于发送队列和去重：account + 会话类型 + 会话 id。
func (s *Session) ChatKey() string {
	return s.Account + ":" + s.ChatType + ":" + itoa(s.ChatID)
}

// FromEvent 把消息事件转成 Session。
func FromEvent(ev *onebot.Event, selfID int64, prefixes []string) *Session {
	s := &Session{
		Account:   ev.Account,
		ChatType:  ev.MessageType,
		UserID:    ev.UserID,
		MessageID: ev.MessageID,
		SelfID:    selfID,
		Nickname:  ev.Nickname,
		Role:      ev.Role,
		Segments:  ev.Segments,
		RawText:   strings.TrimSpace(ev.Text()),
		Mentioned: ev.Mentions(selfID),
		TS:        ev.Time,
	}
	if s.ChatType == "group" {
		s.ChatID = ev.GroupID
	} else {
		s.ChatID = ev.UserID
	}
	s.Text = s.RawText
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(s.Text, p) {
			s.HasPrefix = true
			s.Prefix = p
			s.Text = strings.TrimSpace(strings.TrimPrefix(s.Text, p))
			break
		}
	}
	// 引用消息：reply 段的 id 有的实现发字符串有的发数字，toInt64 都接
	for _, seg := range ev.Segments {
		if seg.Type != "reply" {
			continue
		}
		if id, ok := seg.Data["id"]; ok {
			s.ReplyID = toInt64(id)
			break
		}
	}
	return s
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
