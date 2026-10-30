package send

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xin/onebot/internal/onebot"
)

// 这轮修的竞态：worker 空闲回收和投递同时发生 → 消息进了没人读的队列。

func TestEnqueueNonBlockingWhenChannelFull(t *testing.T) {
	// 用一个小 channel 直接验证"满了就返回 false，不阻塞"。
	// 不能靠 New() 之后塞满——worker 一直在消费，永远塞不满。
	cq := &chatQueue{ch: make(chan job, 2)}
	j := job{ctx: context.Background(), done: make(chan result, 1)}

	select {
	case cq.ch <- j:
	default:
		t.Fatal("空队列应该能塞进去")
	}
	select {
	case cq.ch <- j:
	default:
		t.Fatal("第二个应该能塞进去")
	}
	select {
	case cq.ch <- j:
		t.Fatal("第三个应该满")
	default:
		// 期望走到这里
	}
}

func TestEnqueueConcurrentWithRecycle(t *testing.T) {
	// 大量并发投递 + 回收，不应该 panic 或死锁
	q := New(context.Background(), nil, 0, discardLogger())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "group:" + string(rune('a'+n%5))
			j := job{
				ctx:      context.Background(),
				chatType: "group", chatID: int64(n),
				segments: []onebot.Segment{onebot.Text("x")},
				done:     make(chan result, 1),
			}
			q.enqueue("main", key, j)
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("并发投递死锁了")
	}
}

func TestEnqueueAfterWorkerRemoved(t *testing.T) {
	// worker 从 map 摘除后，新的 enqueue 应该新建一个
	q := New(context.Background(), nil, 0, discardLogger())
	key := "group:9"

	j := job{chatType: "group", chatID: 9, segments: []onebot.Segment{onebot.Text("x")}, done: make(chan result, 1)}
	if !q.enqueue("main", key, j) {
		t.Fatal("应该能投递")
	}

	// 手动摘除（模拟空闲回收）
	q.mu.Lock()
	delete(q.queues, key)
	q.mu.Unlock()

	j2 := job{ctx: context.Background(), chatType: "group", chatID: 9, segments: []onebot.Segment{onebot.Text("y")}, done: make(chan result, 1)}
	if !q.enqueue("main", key, j2) {
		t.Fatal("摘除后应该能新建并投递")
	}
	q.mu.Lock()
	_, ok := q.queues[key]
	q.mu.Unlock()
	if !ok {
		t.Fatal("应该重建了 worker")
	}
}

// discardLogger 是测试用的静默 logger。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestQueueKeyIncludesAccount(t *testing.T) {
	// 同一个群号在不同账号下必须是两个独立队列，
	// 否则会共用 worker 并用错 client。
	q := New(context.Background(), nil, 0, discardLogger())

	key1 := "acc1|group:100"
	key2 := "acc2|group:100"

	j1 := job{ctx: context.Background(), chatType: "group", chatID: 100, done: make(chan result, 1)}
	j2 := job{ctx: context.Background(), chatType: "group", chatID: 100, done: make(chan result, 1)}

	if !q.enqueue("acc1", key1, j1) {
		t.Fatal("acc1 应该能投递")
	}
	if !q.enqueue("acc2", key2, j2) {
		t.Fatal("acc2 应该能投递")
	}

	q.mu.Lock()
	n := len(q.queues)
	q.mu.Unlock()
	if n != 2 {
		t.Fatalf("两个账号应该是两个队列，实际 %d", n)
	}
}

func TestCancelledJobIsNotDelivered(t *testing.T) {
	// **真的起 worker**，投一个已经取消的 job，
	// 断言 worker 跳过发送并回"发送已取消"。
	//
	// 关键：clients 是 nil——如果 worker 真去发了，会拿到
	// "未知账号" 而不是 "发送已取消"。用它区分两条路径。
	q := New(context.Background(), nil, 0, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 调用方超时了

	j := job{
		ctx: ctx, chatType: "group", chatID: 1,
		segments: []onebot.Segment{onebot.Text("x")},
		done:     make(chan result, 1),
	}

	if !q.enqueue("main", "main|group:1", j) {
		t.Fatal("应该能投递")
	}

	select {
	case r := <-j.done:
		if r.err == nil {
			t.Fatal("取消的 job 应该返回错误")
		}
		if !strings.Contains(r.err.Error(), "发送已取消") {
			t.Fatalf("应该是「发送已取消」，实际 %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker 没处理这个 job（可能卡住了）")
	}
}

func TestUncancelledJobReachesDeliver(t *testing.T) {
	// 对照：没取消的 job 会走到 deliver（这里 client 是 nil，
	// 所以拿到"未知账号"——正好证明它没被 ctx 检查拦掉）
	q := New(context.Background(), nil, 0, discardLogger())

	j := job{
		ctx: context.Background(), chatType: "group", chatID: 2,
		segments: []onebot.Segment{onebot.Text("y")},
		done:     make(chan result, 1),
	}
	if !q.enqueue("main", "main|group:2", j) {
		t.Fatal("应该能投递")
	}

	select {
	case r := <-j.done:
		if r.err == nil {
			t.Fatal("client 是 nil，应该报错")
		}
		if strings.Contains(r.err.Error(), "发送已取消") {
			t.Fatalf("没取消的 job 不该报「发送已取消」: %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker 没处理")
	}
}
