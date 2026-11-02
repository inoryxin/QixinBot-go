package main

import (
	"strings"
	"testing"

	"github.com/xin/onebot/internal/onebot"
)

// 回归：多张图要按"单张"判定，不能按整条消息总和卡死。
// 之前 doSend 用 approxSize 求和 > 7MB 直接拒绝：4 张 2.5MB 的图必挂。
func TestSegmentBytesPerImage(t *testing.T) {
	// base64:// 按解码后大小估算（长度 * 3/4）
	payload := "base64://" + strings.Repeat("A", 4<<20) // 4MB 字符 → 3MB 原始
	seg := onebot.Segment{Type: "image", Data: map[string]any{"file": payload}}
	if n := segmentBytes(seg); n < (3<<20)-16 || n > (3<<20)+16 {
		t.Fatalf("base64 估算应约 3MB，实际 %d", n)
	}
	// 普通字段按原长
	text := onebot.Segment{Type: "text", Data: map[string]any{"text": "你好"}}
	if n := segmentBytes(text); n != len("你好") {
		t.Fatalf("文本段估算错误: %d", n)
	}
	// 单张超过上限时能被发现
	big := onebot.Segment{Type: "image", Data: map[string]any{
		"file": "base64://" + strings.Repeat("A", (maxImageBytes/3*4)+1024),
	}}
	if segmentBytes(big) <= maxImageBytes {
		t.Fatal("超限图片应被判定为超过单张上限")
	}
	// 4 张各 3MB 合计 12MB：单张都不超，总量也远低于兜底
	total := 0
	for i := 0; i < 4; i++ {
		total += segmentBytes(seg)
	}
	if total > maxMessageBytes {
		t.Fatalf("4×3MB 不应触发总量兜底（%d）", total)
	}
}
