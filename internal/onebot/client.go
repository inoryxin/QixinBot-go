package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ErrNotConnected 表示当前没有可用的上游连接。
var ErrNotConnected = errors.New("onebot: 尚未连接")

// APIError 是上游返回 retcode != 0 时的错误。
type APIError struct {
	Action  string
	RetCode int64
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("onebot %s 失败 (retcode=%d): %s", e.Action, e.RetCode, e.Message)
}

type apiResp struct {
	Status  string          `json:"status"`
	RetCode int64           `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
}

// Client 是一条 OneBot 11 WebSocket 连接（对应一个账号）。
type Client struct {
	AccountID string

	url   string
	token string
	log   *slog.Logger

	mu   sync.Mutex
	conn *websocket.Conn

	seq    atomic.Int64
	selfID atomic.Int64
	nick   atomic.Value // string

	pendMu  sync.Mutex
	pending map[string]chan *apiResp

	events chan *Event

	// closed 在每次连接关闭时关闭：进行中的 Call 快速失败，
	// 不用等调用方自己的超时（与共享宿主的 failPendingOf 同哲学）。
	closed chan struct{}

	// 心跳：每 interval 发一个 WS ping 并等 pong（等 pong 就是"读超时"——
	// pong 不到 = 连接半死）。失败或超时立即关闭连接，走既有退避重连。
	// 没有这套机制时，对端静默掉线（NAT 超时/拔网线）会让 Read 永远阻塞，
	// bot 表现为"连着但全哑"且永不重连。
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	lastRecv          atomic.Int64 // 最近一次收到任何帧/pong 的时间
	lastEventAt       atomic.Int64 // 最近一次收到业务事件的时间（连接页可见性）
	eventWarned       atomic.Bool
}

// 默认心跳参数：30s 一跳，10s 内没等到 pong 判死。
const (
	defaultHeartbeatInterval = 30 * time.Second
	defaultHeartbeatTimeout  = 10 * time.Second
)

// New 创建一个客户端；此时还没有连接。
func New(accountID, url, token string, log *slog.Logger) *Client {
	return &Client{
		AccountID:         accountID,
		url:               url,
		token:             token,
		log:               log.With("account", accountID),
		pending:           make(map[string]chan *apiResp),
		events:            make(chan *Event, 512),
		closed:            make(chan struct{}),
		HeartbeatInterval: defaultHeartbeatInterval,
		HeartbeatTimeout:  defaultHeartbeatTimeout,
	}
}

// Events 返回事件通道，由上层消费。
func (c *Client) Events() <-chan *Event { return c.events }

// SelfID 返回登录 QQ 号（未登录时为 0）。
func (c *Client) SelfID() int64 { return c.selfID.Load() }

// LastEventAt 返回最近一次收到业务事件的 Unix 纳秒（从未收到过为 0）。
// 连接活着但对端挂起时，这个值不更新——运营可见性的关键。
func (c *Client) LastEventAt() int64 { return c.lastEventAt.Load() }

// LastRecvAt 返回最近一次收到任何 WS 帧/pong 的时间。
func (c *Client) LastRecvAt() int64 { return c.lastRecv.Load() }

// Nickname 返回登录昵称。
func (c *Client) Nickname() string {
	if v := c.nick.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Connected 返回当前是否有活动连接。
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Run 维持连接直到 ctx 取消，断线后指数退避重连。
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := c.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		// 连接稳定超过 30 秒才重置退避，避免"连上就断"时疯狂重试。
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		c.log.Warn("连接断开，准备重连", "err", err, "in", backoff.Round(time.Millisecond))
		jitter := time.Duration(rand.Int63n(int64(backoff) / 5))
		select {
		case <-time.After(backoff + jitter):
		case <-ctx.Done():
			return
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) connect(ctx context.Context) error {
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if c.token != "" {
		opts.HTTPHeader.Set("Authorization", "Bearer "+c.token)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, c.url, opts)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("握手失败 HTTP %d: %w", resp.StatusCode, err)
		}
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.closed = make(chan struct{})
	c.mu.Unlock()
	c.log.Info("已连接上游", "url", c.url)

	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "")
		close(c.closed) // 通知进行中的 Call：连接已死，快速失败
	}()

	// 读循环必须先跑起来，否则下面 get_login_info 的响应没人接收，直接死锁。
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	c.lastRecv.Store(time.Now().UnixNano())
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(connCtx, conn) }()
	go c.heartbeat(connCtx, conn)

	// 登录信息与"连接死亡"竞争：心跳失败会 CloseNow，readLoop 立刻报错——
	// 不能让死连接上的 get_login_info 挂满 10s 拖住重连
	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
	defer callCancel()
	loginDone := make(chan struct{})
	go func() {
		defer close(loginDone)
		if data, err := c.Call(callCtx, "get_login_info", map[string]any{}); err != nil {
			c.log.Warn("get_login_info 失败", "err", err)
		} else {
			var info struct {
				UserID   int64  `json:"user_id"`
				Nickname string `json:"nickname"`
			}
			if json.Unmarshal(data, &info) == nil && info.UserID != 0 {
				c.selfID.Store(info.UserID)
				c.nick.Store(info.Nickname)
				c.log.Info("登录账号", "self_id", info.UserID, "nickname", info.Nickname)
			}
		}
	}()

	select {
	case err := <-readErr:
		// 连接已死（心跳失败/读错误）：立即走重连
		return err
	case <-loginDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	return <-readErr
}

// heartbeat 心跳：定期 ping 并等 pong。失败 = 连接已死（半开/对端无响应），
// 立即 CloseNow 让 readLoop 报错、Run 走既有退避重连。
// pong 的接收依赖 readLoop 在持续读（coder/websocket 在 Read 时处理控制帧），
// readLoop 永远在读，所以 Ping 能等到 pong。
func (c *Client) heartbeat(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(c.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, c.HeartbeatTimeout)
		err := conn.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("心跳失败，关闭连接准备重连", "err", err)
			}
			conn.CloseNow()
			return
		}
		c.lastRecv.Store(time.Now().UnixNano())
		// 连接活着但对端长时间没推送任何业务事件：它可能挂起了
		// （WS 层 pong 正常、业务层不工作）。只警告一次，别刷屏。
		if c.lastEventAt.Load() != 0 && !c.eventWarned.Load() &&
			time.Since(time.Unix(0, c.lastEventAt.Load())) > 30*time.Minute {
			c.eventWarned.Store(true)
			c.log.Warn("对端超过 30 分钟未推送任何业务事件（连接心跳正常）——对端可能已挂起，请检查协议端进程")
		}
	}
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		c.lastRecv.Store(time.Now().UnixNano())
		if typ != websocket.MessageText && typ != websocket.MessageBinary {
			continue
		}

		var probe struct {
			PostType string `json:"post_type"`
			Echo     string `json:"echo"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			c.log.Warn("无法解析的帧", "err", err)
			continue
		}

		// 响应：带 echo，投递给等待中的调用方。
		if probe.Echo != "" {
			var r apiResp
			if err := json.Unmarshal(data, &r); err != nil {
				continue
			}
			c.pendMu.Lock()
			ch := c.pending[probe.Echo]
			c.pendMu.Unlock()
			if ch != nil {
				select {
				case ch <- &r:
				default:
				}
			}
			continue
		}

		if probe.PostType == "" {
			continue
		}
		// 自己发出的消息不能再当输入，否则会自问自答。
		if probe.PostType == "message_sent" {
			continue
		}

		ev, err := ParseEvent(c.AccountID, data)
		if err != nil {
			c.log.Warn("事件解析失败", "err", err)
			continue
		}
		c.lastEventAt.Store(time.Now().UnixNano())
		c.eventWarned.Store(false)
		select {
		case c.events <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Call 调用一个 OneBot action，返回 data 字段。
func (c *Client) Call(ctx context.Context, action string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	echo := "h-" + strconv.FormatInt(c.seq.Add(1), 10)
	ch := make(chan *apiResp, 1)

	c.pendMu.Lock()
	c.pending[echo] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, echo)
		c.pendMu.Unlock()
	}()

	payload, err := json.Marshal(map[string]any{"action": action, "params": params, "echo": echo})
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	conn := c.conn
	// closed 必须在这里一起快照：重连会把 c.closed 整个换掉，
	// 下面的 select 若直接读字段，会和下一次 connect 的赋值构成数据竞争
	closed := c.closed
	c.mu.Unlock()
	if conn == nil {
		return nil, ErrNotConnected
	}

	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, payload); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.RetCode != 0 {
			msg := resp.Wording
			if msg == "" {
				msg = resp.Message
			}
			if msg == "" {
				msg = "未知错误"
			}
			return nil, &APIError{Action: action, RetCode: resp.RetCode, Message: msg}
		}
		return resp.Data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-closed:
		// 连接死亡（心跳失败/读错误）：快速失败，不等调用方自己的超时
		return nil, ErrNotConnected
	}
}

// CallInto 调用并把 data 解析进 out。
func (c *Client) CallInto(ctx context.Context, action string, params, out any) error {
	data, err := c.Call(ctx, action, params)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Close 主动断开当前连接。
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close(websocket.StatusNormalClosure, "shutdown")
		c.conn = nil
	}
}
