// mock-onebot 是一个假的 SnowLuma / OneBot 11 服务端，用来在没有 QQ 的情况下
// 跑通整条链路：连上 → 收事件 → 插件处理 → 发消息回来。
//
//	go run ./tools/mock-onebot -text "/echo 你好"
//
// 它会在新连接建立 2 秒后推一条群消息，并把机器人发回来的消息打印到 stdout。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type mock struct {
	selfID   int64
	groupID  int64
	userID   int64
	token    string
	pushText string
	interval time.Duration

	mu    sync.Mutex
	conns []*websocket.Conn

	eventID atomic.Int64
	msgID   atomic.Int64
}

func main() {
	addr := flag.String("addr", "127.0.0.1:3001", "监听地址")
	selfID := flag.Int64("self", 10000, "机器人 QQ 号")
	groupID := flag.Int64("group", 123456, "群号")
	userID := flag.Int64("user", 20000, "发送者 QQ 号")
	token := flag.String("token", "", "访问令牌（留空不校验）")
	text := flag.String("text", "/echo 你好", "要推送的消息文本")
	interval := flag.Duration("interval", 0, "重复推送间隔（0 = 只推一次）")
	flag.Parse()

	m := &mock{
		selfID:   *selfID,
		groupID:  *groupID,
		userID:   *userID,
		token:    *token,
		pushText: *text,
		interval: *interval,
	}
	// message_id 必须跨重启单调，否则客户端会把它当成重复事件丢掉。
	m.eventID.Store(time.Now().UnixNano() / 1000)

	http.HandleFunc("/", m.handle)
	// 手动推一条：curl "http://127.0.0.1:3001/push?text=/help"
	http.HandleFunc("/push", m.handlePush)
	log.Printf("mock OneBot 监听 %s（self=%d group=%d）", *addr, *selfID, *groupID)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func (m *mock) handlePush(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		http.Error(w, "missing text", http.StatusBadRequest)
		return
	}
	withAt := r.URL.Query().Get("at") == "1"
	withImage := r.URL.Query().Get("image")
	isPrivate := r.URL.Query().Get("private") == "1"

	// 支持指定群和用户，方便测"两个群同时来消息"
	groupID := m.groupID
	userID := m.userID
	if v := r.URL.Query().Get("group"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			groupID = n
		}
	}
	if v := r.URL.Query().Get("user"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			userID = n
		}
	}

	m.mu.Lock()
	conns := append([]*websocket.Conn(nil), m.conns...)
	m.mu.Unlock()
	for _, c := range conns {
		if err := m.pushTo(c, text, withAt, withImage, isPrivate, groupID, userID); err != nil {
			log.Printf("推送失败: %v", err)
		}
	}
	_, _ = w.Write([]byte("pushed\n"))
}

func (m *mock) handle(w http.ResponseWriter, r *http.Request) {
	if m.token != "" {
		if r.Header.Get("Authorization") != "Bearer "+m.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("握手失败: %v", err)
		return
	}
	// 图片/语音 base64 可能远超默认 32KB；提高上限，避免 dev 里大消息把连接顶掉
	conn.SetReadLimit(16 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()

	log.Printf("客户端已连接: %s", r.RemoteAddr)

	go m.readLoop(conn)
	go m.pushLoop(conn)

	<-r.Context().Done()

	m.mu.Lock()
	for i, c := range m.conns {
		if c == conn {
			m.conns = append(m.conns[:i], m.conns[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	log.Printf("客户端断开: %s", r.RemoteAddr)
}

func (m *mock) readLoop(conn *websocket.Conn) {
	ctx := context.Background()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var req struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
			Echo   string         `json:"echo"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			log.Printf("收到无法解析的帧: %s", string(data))
			continue
		}
		m.handleAction(conn, req.Echo, req.Action, req.Params)
	}
}

func (m *mock) handleAction(conn *websocket.Conn, echo, action string, params map[string]any) {
	var data any = map[string]any{}

	switch action {
	case "get_login_info":
		data = map[string]any{"user_id": m.selfID, "nickname": "MockBot"}
	case "get_status":
		data = map[string]any{"online": true, "good": true}
	case "get_version_info":
		data = map[string]any{"app_name": "mock-onebot", "app_version": "0.1.0", "protocol_version": "v11"}
	case "send_group_msg":
		log.Printf("[发送 → 群 %v] %s", params["group_id"], textOf(params["message"]))
		data = map[string]any{"message_id": m.msgID.Add(1)}
	case "send_private_msg":
		log.Printf("[发送 → 私聊 %v] %s", params["user_id"], textOf(params["message"]))
		data = map[string]any{"message_id": m.msgID.Add(1)}
	case "get_group_list":
		data = []any{
			map[string]any{"group_id": 123456, "group_name": "测试群", "member_count": 3},
			map[string]any{"group_id": 654321, "group_name": "摸鱼群", "member_count": 12},
		}
	case "get_friend_list":
		data = []any{
			map[string]any{"user_id": 20000, "nickname": "测试用户", "remark": ""},
			map[string]any{"user_id": 2849446053, "nickname": "主人", "remark": ""},
		}
	case "delete_msg":
		log.Printf("[撤回] %v", params["message_id"])
	default:
		log.Printf("[未处理 action] %s", action)
	}

	resp := map[string]any{
		"status":  "ok",
		"retcode": 0,
		"data":    data,
		"echo":    echo,
	}
	b, _ := json.Marshal(resp)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, b)
}

func (m *mock) pushLoop(conn *websocket.Conn) {
	time.Sleep(2 * time.Second)

	for {
		if err := m.push(conn, m.pushText, false, "", false); err != nil {
			return
		}
		if m.interval <= 0 {
			return
		}
		time.Sleep(m.interval)
	}
}

func (m *mock) push(conn *websocket.Conn, text string, withAt bool, withImage string, isPrivate bool) error {
	return m.pushTo(conn, text, withAt, withImage, isPrivate, m.groupID, m.userID)
}

// pushTo 推一条消息到指定群和用户（测"两个群同时来"用）。
func (m *mock) pushTo(conn *websocket.Conn, text string, withAt bool, withImage string, isPrivate bool, groupID, userID int64) error {
	msgType := "group"
	if isPrivate {
		msgType = "private"
	}
	ev := map[string]any{
		"post_type":    "message",
		"message_type": msgType,
		"self_id":      m.selfID,
		"user_id":      userID,
		"message_id":   m.eventID.Add(1),
		"time":         time.Now().Unix(),
		"message": func() []any {
			segs := []any{}
			if withAt {
				segs = append(segs, map[string]any{"type": "at", "data": map[string]any{"qq": fmt.Sprintf("%d", m.selfID)}})
			}
			if withImage != "" {
				segs = append(segs, map[string]any{"type": "image", "data": map[string]any{"url": withImage}})
			}
			if text != "" {
				segs = append(segs, map[string]any{"type": "text", "data": map[string]any{"text": text}})
			}
			return segs
		}(),
		"sender": map[string]any{
			"user_id":  userID,
			"nickname": "测试用户",
			"card":     "",
			"role":     "member",
		},
	}
	if !isPrivate {
		ev["group_id"] = groupID
	}
	b, _ := json.Marshal(ev)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log.Printf("推送事件: %q", text)
	return conn.Write(ctx, websocket.MessageText, b)
}

func textOf(v any) string {
	switch msg := v.(type) {
	case string:
		return msg
	case []any:
		var out string
		for _, item := range msg {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if obj["type"] == "text" {
				if d, ok := obj["data"].(map[string]any); ok {
					if s, ok := d["text"].(string); ok {
						out += s
					}
				}
			} else {
				out += fmt.Sprintf("[%v]", obj["type"])
			}
		}
		return out
	default:
		return fmt.Sprintf("%v", v)
	}
}
