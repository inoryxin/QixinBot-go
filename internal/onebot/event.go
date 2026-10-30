package onebot

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Segment 是 OneBot 11 消息段：{"type":"text","data":{"text":"hi"}}。
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// Text 是构造纯文本段的快捷方法。
func Text(s string) Segment {
	return Segment{Type: "text", Data: map[string]any{"text": s}}
}

// At 构造 @ 段。
func At(qq int64) Segment {
	return Segment{Type: "at", Data: map[string]any{"qq": strconv.FormatInt(qq, 10)}}
}

// Image 构造图片段，file 可以是 URL、base64:// 或本地路径。
func Image(file string) Segment {
	return Segment{Type: "image", Data: map[string]any{"file": file}}
}

// Event 是归一化后的 OneBot 事件。常用字段提出来，原始数据保留在 Raw。
type Event struct {
	Account string
	Raw     map[string]any

	PostType    string
	MessageType string
	NoticeType  string
	SubType     string
	MetaType    string

	SelfID    int64
	UserID    int64
	GroupID   int64
	MessageID int64
	TargetID  int64
	Time      int64

	Message  any
	Sender   map[string]any
	Nickname string
	Role     string

	Segments []Segment
}

// ParseEvent 把一帧原始 JSON 变成 Event。
func ParseEvent(account string, data []byte) (*Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	ev := &Event{Account: account, Raw: raw}
	ev.PostType = str(raw["post_type"])
	ev.MessageType = str(raw["message_type"])
	ev.NoticeType = str(raw["notice_type"])
	ev.SubType = str(raw["sub_type"])
	ev.MetaType = str(raw["meta_event_type"])
	ev.SelfID = num(raw["self_id"])
	ev.UserID = num(raw["user_id"])
	ev.GroupID = num(raw["group_id"])
	ev.MessageID = num(raw["message_id"])
	ev.TargetID = num(raw["target_id"])
	ev.Time = num(raw["time"])
	ev.Message = raw["message"]

	if s, ok := raw["sender"].(map[string]any); ok {
		ev.Sender = s
		ev.Nickname = str(s["card"])
		if ev.Nickname == "" {
			ev.Nickname = str(s["nickname"])
		}
		ev.Role = str(s["role"])
	}
	if ev.Message != nil {
		ev.Segments = ParseMessage(ev.Message)
	}
	return ev, nil
}

// ParseMessage 兼容两种格式：消息段数组，或用 CQ 码拼成的字符串。
func ParseMessage(v any) []Segment {
	switch m := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]Segment, 0, len(m))
		for _, item := range m {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			data, _ := obj["data"].(map[string]any)
			out = append(out, Segment{Type: str(obj["type"]), Data: data})
		}
		return out
	case []Segment:
		return m
	case string:
		return parseCQ(m)
	default:
		return nil
	}
}

var cqRe = regexp.MustCompile(`\[CQ:([a-zA-Z_]+)((?:,[^\]]*)?)\]`)

// parseCQ 只解析常用的几种 CQ 码；未知类型原样保留成 text，不丢内容。
func parseCQ(s string) []Segment {
	if !strings.Contains(s, "[CQ:") {
		return []Segment{Text(unescapeCQ(s))}
	}
	var out []Segment
	last := 0
	for _, loc := range cqRe.FindAllStringSubmatchIndex(s, -1) {
		if loc[0] > last {
			out = append(out, Text(unescapeCQ(s[last:loc[0]])))
		}
		typ := s[loc[2]:loc[3]]
		data := map[string]any{}
		if loc[4] >= 0 && loc[5] > loc[4] {
			for _, kv := range strings.Split(strings.TrimPrefix(s[loc[4]:loc[5]], ","), ",") {
				if k, v, ok := strings.Cut(kv, "="); ok {
					data[k] = unescapeCQ(v)
				}
			}
		}
		out = append(out, Segment{Type: typ, Data: data})
		last = loc[1]
	}
	if last < len(s) {
		out = append(out, Text(unescapeCQ(s[last:])))
	}
	return out
}

func unescapeCQ(s string) string {
	if !strings.Contains(s, "&#") {
		return s
	}
	r := strings.NewReplacer("&#91;", "[", "&#93;", "]", "&#44;", ",", "&amp;", "&")
	return r.Replace(s)
}

// Text 把所有 text 段拼起来。
func (e *Event) Text() string {
	var b strings.Builder
	for _, s := range e.Segments {
		if s.Type == "text" {
			b.WriteString(str(s.Data["text"]))
		}
	}
	return b.String()
}

// Mentions 判断消息里是否 @ 了指定 QQ。
func (e *Event) Mentions(qq int64) bool {
	if qq == 0 {
		return false
	}
	target := strconv.FormatInt(qq, 10)
	for _, s := range e.Segments {
		if s.Type == "at" && str(s.Data["qq"]) == target {
			return true
		}
	}
	return false
}

// ReplyTo 返回被引用消息的 id（没有则为 0）。
func (e *Event) ReplyTo() int64 {
	for _, s := range e.Segments {
		if s.Type == "reply" {
			return num(s.Data["id"])
		}
	}
	return 0
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return 0
	}
}
