package onebot

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// 心跳验收：
//   1. 对端装死（不读不回 pong）→ 心跳超时 → 客户端必须重连（服务端看到第二条连接）
//   2. 对端活着（在读，库自动回 pong）→ 连接保持，不误杀
// 用可调的短心跳参数（300ms/300ms）避免真实 30s 等待。

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startTestListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func wsURL(ln net.Listener) string { return "ws://" + ln.Addr().String() }

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c := New("main", url, "", testLogger())
	c.HeartbeatInterval = 300 * time.Millisecond
	c.HeartbeatTimeout = 300 * time.Millisecond
	return c
}

func TestHeartbeatDetectsDeadPeer(t *testing.T) {
	var conns atomic.Int32
	srv := &http.Server{}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n := conns.Add(1)
		ctx := r.Context()
		// 第一条连接装死（不读 → 客户端的 ping 永远等不到 pong）；
		// 重连后的连接正常读（证明客户端恢复后连接稳定）。
		if n == 1 {
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	})
	ln := startTestListener(t)
	go srv.Serve(ln)
	defer srv.Close()

	c := newTestClient(t, wsURL(ln))
	ctx := context.Background()
	go c.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	deadline := time.Now().Add(10 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if conns.Load() < 2 {
		t.Fatalf("心跳应触发对装死对端的重连（服务端应看到第二条连接），实际 %d 条", conns.Load())
	}
	time.Sleep(600 * time.Millisecond) // 稳定观察窗口
	if n := conns.Load(); n > 3 {
		t.Fatalf("重连后应稳定，实际 %d 条连接", n)
	}
	c.mu.Lock()
	alive := c.conn != nil
	c.mu.Unlock()
	if !alive {
		t.Fatal("重连后客户端应有活动连接")
	}
}

func TestHeartbeatKeepsAliveOnLivePeer(t *testing.T) {
	var conns atomic.Int32
	srv := &http.Server{}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conns.Add(1)
		ctx := r.Context()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	})
	ln := startTestListener(t)
	go srv.Serve(ln)
	defer srv.Close()

	c := newTestClient(t, wsURL(ln))
	ctx := context.Background()
	go c.Run(ctx)
	time.Sleep(2500 * time.Millisecond) // 覆盖 ~7 个心跳周期
	if conns.Load() != 1 {
		t.Fatalf("活对端不应触发重连，服务端看到 %d 条连接", conns.Load())
	}
	if !c.Connected() {
		t.Fatal("客户端应保持连接")
	}
}
