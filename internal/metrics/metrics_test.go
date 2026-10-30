package metrics

// 回归测试：首窗必须有真实的 Start。
// New 不设 cur.start 的话，启动后前 5 分钟的数据落在 Start=0 的窗口里，
// Snapshot 按 Start!=0 过滤会把它永久丢掉。

import (
	"testing"
	"time"
)

func TestFirstWindowKept(t *testing.T) {
	c := New(nil)
	c.MsgIn()
	c.PluginCall(5, false)
	c.roll() // 手动滚窗（不等到 5 分钟）

	ws := c.Snapshot()
	if len(ws) == 0 {
		t.Fatal("首窗数据被丢弃")
	}
	if ws[0].Start <= 0 {
		t.Fatalf("首窗 Start 应为真实时间戳，得到 %d", ws[0].Start)
	}
	if time.Since(time.Unix(ws[0].Start, 0)) > 2*time.Minute {
		t.Fatalf("首窗 Start 应接近现在，得到 %v 前", time.Since(time.Unix(ws[0].Start, 0)))
	}
	if ws[0].MsgIn != 1 || ws[0].Calls != 1 || ws[0].AvgMS != 5 {
		t.Fatalf("首窗聚合值不对: %+v", ws[0])
	}
}

func TestEmptyWindowsNotKept(t *testing.T) {
	c := New(nil)
	c.roll() // 空窗口
	if len(c.Snapshot()) != 0 {
		t.Fatal("全空窗口不应进历史（半夜不该有一整排 0）")
	}
}
