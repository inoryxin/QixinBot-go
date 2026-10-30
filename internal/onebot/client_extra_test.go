package onebot

// WS 客户端行为验收：重连、调用超时、事件分发。
// 这些之前零测试——2026-10-28 的"对端假活挂一天"事故就出在这一层。

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// serveWS 起一个最小 WS 服务端：每条连接交给 onConn（i = 第几条连接，从 0 起）。
// 返回 ws:// 地址和连接计数。
func serveWS(t *testing.T, onConn func(conn *websocket.Conn, i int)) (string, *atomic.Int32) {
	t.Helper()
	ln := startTestListener(t)
	count := new(atomic.Int32)
	srv := &http.Server{}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		i := int(count.Add(1)) - 1
		onConn(conn, i)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return wsURL(ln), count
}

// waitConnected 等客户端连上（Connected 为真）。
func waitConnected(t *testing.T, c *Client, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.Connected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("连接没有就绪")
}

// TestReconnectAfterDrop：对端主动断开 → Run 必须自动重连（服务端看到第二条连接）。
func TestReconnectAfterDrop(t *testing.T) {
	url, seen := serveWS(t, func(conn *websocket.Conn, i int) {
		if i == 0 {
			// 第一条连接：握手成功后立刻断
			_ = conn.Close(websocket.StatusGoingAway, "bye")
			time.Sleep(3 * time.Second) // handler 返回即关连接，挂住第二条
			return
		}
		time.Sleep(3 * time.Second)
	})

	c := newTestClient(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if seen.Load() >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("对端断开后没有重连，服务端只见到 %d 条连接", seen.Load())
}

// TestCallDeadline：服务端收下请求但永不回 → Call 必须在 ctx 截止时返回，不挂死。
func TestCallDeadline(t *testing.T) {
	url, _ := serveWS(t, func(conn *websocket.Conn, i int) {
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	})

	c := newTestClient(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)
	waitConnected(t, c, 3*time.Second)

	callCtx, callCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer callCancel()
	start := time.Now()
	_, err := c.Call(callCtx, "get_status", map[string]any{})
	if err == nil {
		t.Fatal("对端不回包，Call 竟然成功了")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Call 没有按 ctx 截止返回（耗时 %v）", time.Since(start))
	}
}

// TestEventParsed：服务端推一条事件帧 → Events() 必须吐出解析好的事件。
func TestEventParsed(t *testing.T) {
	url, _ := serveWS(t, func(conn *websocket.Conn, i int) {
		// 收掉 get_login_info 请求再推事件，避免读缓冲互占
		go func() {
			for {
				if _, _, err := conn.Read(context.Background()); err != nil {
					return
				}
			}
		}()
		frame := map[string]any{
			"post_type": "message", "message_type": "group",
			"group_id": 123, "user_id": 456, "message_id": 1,
			"self_id": 10000, "time": time.Now().Unix(),
			"message": []map[string]any{{"type": "text", "data": map[string]any{"text": "hello"}}},
		}
		raw, _ := json.Marshal(frame)
		wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer wcancel()
		_ = conn.Write(wctx, websocket.MessageText, raw)
		time.Sleep(1500 * time.Millisecond)
	})

	c := newTestClient(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.After(4 * time.Second)
	for {
		select {
		case ev := <-c.Events():
			if ev.PostType == "message" && ev.GroupID == 123 {
				return
			}
		case <-deadline:
			t.Fatal("事件帧没有被解析进 Events()")
		case <-ctx.Done():
			t.Fatal("client 提前退出")
		}
	}
}
