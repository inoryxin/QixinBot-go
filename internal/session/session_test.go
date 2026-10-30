package session

import (
	"encoding/json"
	"testing"

	"github.com/xin/onebot/internal/onebot"
)

// 前缀是「下限」不是「上限」：带不带都能触发，取决于 require_prefix。
// 这里只测 session 层怎么剥前缀。
func TestFromEventStripsOnePrefix(t *testing.T) {
	ev := baseEvent("/echo 你好")
	s := FromEvent(ev, 1, []string{"/"})

	if !s.HasPrefix {
		t.Fatal("应该标记 HasPrefix")
	}
	if s.Text != "echo 你好" {
		t.Fatalf("前缀应该被剥掉，实际 %q", s.Text)
	}
	if s.RawText != "/echo 你好" {
		t.Fatal("RawText 应该保留原文")
	}
}

func TestFromEventOnlyStripsOne(t *testing.T) {
	// "//echo" 只剥一个斜杠，剩下 "/echo"，不会变成 "echo"
	ev := baseEvent("//echo")
	s := FromEvent(ev, 1, []string{"/"})

	if s.Text != "/echo" {
		t.Fatalf("只该剥一个前缀，实际 %q", s.Text)
	}
}

func TestFromEventNoPrefix(t *testing.T) {
	ev := baseEvent("echo 你好")
	s := FromEvent(ev, 1, []string{"/"})

	if s.HasPrefix {
		t.Fatal("没前缀不该标记")
	}
	if s.Text != "echo 你好" {
		t.Fatalf("没前缀不该改文本，实际 %q", s.Text)
	}
}

func TestFromEventMultiplePrefixes(t *testing.T) {
	// 配了三个前缀，命中哪个剥哪个
	for _, raw := range []string{"/echo", "!echo", "#echo"} {
		ev := baseEvent(raw)
		s := FromEvent(ev, 1, []string{"/", "!", "#"})
		if s.Text != "echo" {
			t.Fatalf("%q 应该剥成 echo，实际 %q", raw, s.Text)
		}
	}
}

func TestFromEventTrailingSpaceTrimmed(t *testing.T) {
	ev := baseEvent("/echo   你好  ")
	s := FromEvent(ev, 1, []string{"/"})
	if s.Text != "echo   你好" {
		t.Fatalf("应该只去掉前缀和一个多余空格，实际 %q", s.Text)
	}
}

// baseEvent 造一条最简单的私聊文本消息，够测前缀逻辑用。
func baseEvent(text string) *onebot.Event {
	return &onebot.Event{
		PostType:    "message",
		MessageType: "private",
		UserID:      42,
		SelfID:      1,
		MessageID:   100,
		Segments: []onebot.Segment{
			{Type: "text", Data: map[string]any{"text": text}},
		},
	}
}

// ---- 引用/回复上下文 ----

// reply 段的 id 有的实现发字符串，有的发数字，都要接住。
func TestFromEventExtractsReplyID(t *testing.T) {
	ev := baseEvent("回复内容")
	ev.Segments = []onebot.Segment{
		{Type: "reply", Data: map[string]any{"id": "998877"}},
		{Type: "text", Data: map[string]any{"text": "回复内容"}},
	}
	s := FromEvent(ev, 1, nil)
	if s.ReplyID != 998877 {
		t.Fatalf("reply_id 应该是 998877，实际 %d", s.ReplyID)
	}

	ev.Segments[0].Data["id"] = int64(123456)
	if s = FromEvent(ev, 1, nil); s.ReplyID != 123456 {
		t.Fatalf("数字 id 也应该能解析，实际 %d", s.ReplyID)
	}

	// 没有 reply 段：保持零值，别乱填
	if s = FromEvent(baseEvent("普通消息"), 1, nil); s.ReplyID != 0 {
		t.Fatalf("普通消息不该有 reply_id，实际 %d", s.ReplyID)
	}
}

func TestParseReplyResponse(t *testing.T) {
	// 标准段数组形式（多数实现）
	rc := ParseReplyResponse(json.RawMessage(`{
		"message_id": 998877,
		"sender": {"user_id": 666, "nickname": "小明"},
		"message": [
			{"type": "text", "data": {"text": " 被引用的原文 "}},
			{"type": "image", "data": {"url": "http://x/img.jpg"}}
		]
	}`))
	if rc == nil {
		t.Fatal("段数组形式应该解析成功")
	}
	if rc.UserID != 666 || rc.Nickname != "小明" {
		t.Fatalf("sender 解析错了: %+v", rc)
	}
	if rc.Text != "被引用的原文" {
		t.Fatalf("文本应该拼接并去空白，实际 %q", rc.Text)
	}
	if len(rc.Segments) != 2 {
		t.Fatalf("原始段应该保留，实际 %d 段", len(rc.Segments))
	}

	// CQ 码字符串形式（部分实现）
	rc = ParseReplyResponse(json.RawMessage(`{
		"sender": {"user_id": 777},
		"message": "早上好 [CQ:face,id=1]"
	}`))
	if rc == nil || rc.UserID != 777 {
		t.Fatalf("CQ 码形式应该解析成功: %+v", rc)
	}
	if rc.Text != "早上好" {
		t.Fatalf("CQ 码形式文本解析错误: %q", rc.Text)
	}
	if len(rc.Segments) != 2 {
		t.Fatalf("CQ 码应该解析成段: %d", len(rc.Segments))
	}

	// 坏 JSON：返回 nil，调用方只保留 reply_id
	if rc := ParseReplyResponse(json.RawMessage(`{oops`)); rc != nil {
		t.Fatal("坏 JSON 应该返回 nil")
	}

	// data:null（有的实现用 retcode=0 + null data 表示失败）：
	// 必须返回 nil，否则插件拿到一个 truthy 的空对象，if (session.reply) 走错分支
	if rc := ParseReplyResponse(json.RawMessage(`null`)); rc != nil {
		t.Fatal("null data 应该返回 nil")
	}
	if rc := ParseReplyResponse(json.RawMessage(`{}`)); rc != nil {
		t.Fatal("空对象 data 应该返回 nil")
	}

	// sender.user_id 发成字符串：不能让整条引用跟着解析失败
	rc = ParseReplyResponse(json.RawMessage(`{
		"sender": {"user_id": "888", "nickname": "阿花"},
		"message": "被引用"
	}`))
	if rc == nil || rc.UserID != 888 || rc.Nickname != "阿花" {
		t.Fatalf("字符串 user_id 应该宽容解析: %+v", rc)
	}
}
