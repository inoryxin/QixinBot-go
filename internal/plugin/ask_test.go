package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xin/onebot/internal/session"
)

// 交互等待和取消是最容易把用户"卡住"的地方。

func TestAskRegistryDeliverOnce(t *testing.T) {
	r := newAskRegistry()
	ch, _, cancel := r.Register("p", "main", "group", 100, 1)
	defer cancel()

	if !r.Deliver("main", "group", 100, 1, "答案") {
		t.Fatal("应该投递成功")
	}
	if got := <-ch; got != "答案" {
		t.Fatalf("投递的内容不对: %q", got)
	}
	// 第二次投递：等待已经被移除
	if r.Deliver("main", "group", 100, 1, "再来一次") {
		t.Fatal("一次只能答一次")
	}
}

func TestAskRegistryWrongUser(t *testing.T) {
	r := newAskRegistry()
	_, _, cancel := r.Register("p", "main", "group", 100, 1)
	defer cancel()

	// 别人的消息不该被消费
	if r.Deliver("main", "group", 100, 999, "别人的话") {
		t.Fatal("不该消费别人的消息")
	}
	if !r.Deliver("main", "group", 100, 1, "我的话") {
		t.Fatal("本人的消息应该被消费")
	}
}

func TestAskRegistryCancelOne(t *testing.T) {
	r := newAskRegistry()
	_, _, cancel := r.Register("p", "main", "group", 100, 1)
	defer cancel()

	if r.CancelOne("main", "group", 100, 999) {
		t.Fatal("没在等的人不该被取消")
	}
	if !r.CancelOne("main", "group", 100, 1) {
		t.Fatal("本人应该能取消")
	}
	if r.Deliver("main", "group", 100, 1, "迟到的答案") {
		t.Fatal("取消后不该再消费")
	}
}

func TestAskRegistryCancelPlugin(t *testing.T) {
	r := newAskRegistry()
	_, _, c1 := r.Register("p1", "main", "group", 100, 1)
	_, _, c2 := r.Register("p1", "main", "group", 100, 2)
	_, _, c3 := r.Register("p2", "main", "group", 100, 3)
	defer c1()
	defer c2()
	defer c3()

	if n := r.CancelPlugin("p1"); n != 2 {
		t.Fatalf("应该清掉 2 个，实际 %d", n)
	}
	// p2 的不受影响
	if !r.Deliver("main", "group", 100, 3, "还在") {
		t.Fatal("别的插件的等待不该被清")
	}
}

func TestIsCancelWord(t *testing.T) {
	yes := []string{"取消", "cancel", "退出", "算了", "不弄了"}
	no := []string{"", "取消订单", "好的", "1"}

	for _, w := range yes {
		if !IsCancelWord(w) {
			t.Errorf("%q 应该算取消", w)
		}
	}
	for _, w := range no {
		if IsCancelWord(w) {
			t.Errorf("%q 不该算取消", w)
		}
	}
}

func TestAskTimeoutReturnsEmpty(t *testing.T) {
	m := &Manager{asks: newAskRegistry()}
	s := &session.Session{ChatType: "group", ChatID: 1, UserID: 2}

	// 1 毫秒超时，没人回答
	answer, err := m.Ask(context.Background(), "p", "main", "group", 1, 2, 1)
	if err != nil {
		t.Fatalf("超时不该报错: %v", err)
	}
	if answer != "" {
		t.Fatalf("超时应该返回空串，实际 %q", answer)
	}
	_ = s
}

func TestAskContextCanceled(t *testing.T) {
	m := &Manager{asks: newAskRegistry()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.Ask(ctx, "p", "main", "group", 1, 2, 60)
	if err == nil {
		t.Fatal("ctx 取消应该返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应该是 context.Canceled，实际 %v", err)
	}
}

// 用户按取消后，Ask 必须立刻返回，而不是挂到超时。
func TestAskCancelUnblocks(t *testing.T) {
	m := &Manager{asks: newAskRegistry()}
	done := make(chan string, 1)
	go func() {
		text, _ := m.Ask(context.Background(), "p", "main", "private", 1, 1, 5*time.Second)
		done <- text
	}()
	time.Sleep(50 * time.Millisecond)
	if !m.asks.CancelOne("main", "private", 1, 1) {
		t.Fatal("应该能取消当前等待")
	}
	select {
	case text := <-done:
		if text != "" {
			t.Fatalf("取消应返回空字符串，得到 %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后 Ask 没有立刻返回（仍挂到超时）")
	}
}

// 同一用户重新登记等待时，旧的等待必须被取消通知。
func TestAskRegisterOverwriteCancelsOld(t *testing.T) {
	r := newAskRegistry()
	_, oldCancelled, _ := r.Register("p", "main", "group", 100, 1)
	_, _, cancel2 := r.Register("p", "main", "group", 100, 1)
	defer cancel2()

	select {
	case <-oldCancelled:
	default:
		t.Fatal("旧等待应收到取消信号")
	}
}

// host.ask 的注册路径也必须能收到取消信号。
func TestRegisterAskCancelSignal(t *testing.T) {
	m := &Manager{asks: newAskRegistry()}
	_, cancelled, cancel := m.RegisterAsk("p", "main", "private", 1, 1)
	defer cancel()

	if !m.asks.CancelOne("main", "private", 1, 1) {
		t.Fatal("应该能取消")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("RegisterAsk 的取消信号没有关闭")
	}
}
