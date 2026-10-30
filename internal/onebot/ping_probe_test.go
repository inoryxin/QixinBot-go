package onebot

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// 探针：服务端 handler 不读时，客户端 conn.Ping 能不能等到 pong？
func TestProbePingAgainstNonReadingServer(t *testing.T) {
	srv := &http.Server{}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// 故意不读、不回任何东西
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		_ = conn
	})
	ln := startTestListener(t)
	go srv.Serve(ln)
	defer srv.Close()

	ctx := context.Background()
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	clientConn, _, err := websocket.Dial(dialCtx, wsURL(ln), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.CloseNow()

	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = clientConn.Ping(pingCtx)
	t.Logf("Ping 结果: err=%v, 耗时=%v", err, time.Since(start))
	if err == nil {
		t.Log("服务端不读也回了 pong —— coder/websocket 在传输层自动回 pong，Ping 无法探测装死对端")
	} else {
		t.Log("Ping 超时失败 —— 心跳方案成立")
	}
}
