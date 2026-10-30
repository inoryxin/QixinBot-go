package session

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/xin/onebot/internal/onebot"
)

// EventPayload 是交给插件的非消息事件（通知、请求、元事件）。
// 字段名就是协议名，插件直接读。
type EventPayload struct {
	Account string `json:"account"`

	PostType      string `json:"post_type"`
	NoticeType    string `json:"notice_type,omitempty"`
	RequestType   string `json:"request_type,omitempty"`
	MetaEventType string `json:"meta_event_type,omitempty"`
	SubType       string `json:"sub_type,omitempty"`

	SelfID     int64 `json:"self_id"`
	UserID     int64 `json:"user_id,omitempty"`
	GroupID    int64 `json:"group_id,omitempty"`
	MessageID  int64 `json:"message_id,omitempty"`
	TargetID   int64 `json:"target_id,omitempty"`
	OperatorID int64 `json:"operator_id,omitempty"`
	Time       int64 `json:"time,omitempty"`

	// Raw 保留原始事件，插件需要扩展字段时自己取。
	Raw map[string]any `json:"raw,omitempty"`
}

// FromOneBotEvent 把 OneBot 事件转成插件可读的 payload。
func FromOneBotEvent(ev *onebot.Event) *EventPayload {
	if ev == nil {
		return nil
	}
	p := &EventPayload{
		Account:       ev.Account,
		PostType:      ev.PostType,
		NoticeType:    ev.NoticeType,
		SubType:       ev.SubType,
		MetaEventType: ev.MetaType,
		SelfID:        ev.SelfID,
		UserID:        ev.UserID,
		GroupID:       ev.GroupID,
		MessageID:     ev.MessageID,
		TargetID:      ev.TargetID,
		Time:          ev.Time,
		Raw:           ev.Raw,
	}
	if rt, ok := ev.Raw["request_type"].(string); ok {
		p.RequestType = rt
	}
	if op, ok := ev.Raw["operator_id"]; ok {
		p.OperatorID = toInt64(op)
	}
	return p
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		// 有的实现把 ID 发成字符串/Number；和 onebot.num 的宽容度对齐，
		// 否则 operator_id 静默变 0，授权判断跟着错
		if n, err := x.Int64(); err == nil {
			return n
		}
		return 0
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}
