// Package chatbuf 保存最近的收发消息，给 WebUI 的聊天页用。
// 落盘到 data/chat.json，重启后侧栏还能看到最近聊过的会话。
//
// 落盘是节流的：首个脏写入后 1 秒统一刷一次盘（固定定时器，不随后续
// 写入顺延——持续刷屏时每秒至多一次全量写，任意时刻崩溃的丢失上限
// 都是 1 秒）。之前每条消息都全量序列化 + 重命名一遍，热群里等于每条
// 消息一次全文件写。
//
// 并发落盘有顺序保证：flush（定时器 goroutine）和 Flush（关机路径）
// 可能同时拿快照，writeFile 用 fileMu 串行写盘 + savedGen 跳过旧快照，
// 磁盘上的内容永远不会比内存里的旧。
package chatbuf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// saveDelay 是落盘节流窗口。
const saveDelay = time.Second

// Message 是一条收发的消息。
type Message struct {
	ID        int64    `json:"id"`
	TS        int64    `json:"ts"`
	Direction string   `json:"direction"` // in | out
	Account   string   `json:"account"`
	ChatType  string   `json:"chat_type"`
	ChatID    int64    `json:"chat_id"`
	UserID    int64    `json:"user_id,omitempty"`
	Nickname  string   `json:"nickname,omitempty"`
	Plugin    string   `json:"plugin,omitempty"`
	Text      string   `json:"text"`
	Images    []string `json:"images,omitempty"` // 图片 URL，给前端渲染
}

type Buffer struct {
	mu    sync.Mutex
	msgs  []Message
	limit int
	path  string
	dirty bool
	timer *time.Timer // 节流定时器（mu 保护）

	// 落盘串行化与顺序保证：fileMu 让同一时刻只有一个 goroutine 在写
	// tmp 文件；savedGen 是已落盘快照的变更代数（每次 Add/Annotate 自增），
	// 旧快照直接跳过，杜绝"flush 的旧快照把 Flush 的新快照覆盖掉"。
	// 不能用最大消息 id 判新旧：Annotate 只改字段不产生新 id，
	// 注释完的最后一条消息会被判成"旧快照"永远不落盘。
	fileMu   sync.Mutex
	savedGen atomic.Int64
	gen      int64

	nextID atomic.Int64
}

// New 创建缓冲。path 非空时从磁盘恢复历史，写入后节流落盘。
func New(limit int, path string) *Buffer {
	if limit <= 0 {
		limit = 500
	}
	b := &Buffer{limit: limit, path: path}
	b.load()
	return b
}

func (b *Buffer) load() {
	if b.path == "" {
		return
	}
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return
	}
	var list []Message
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	if len(list) > b.limit {
		list = list[len(list)-b.limit:]
	}
	b.msgs = list
	var maxID int64
	for _, m := range list {
		if m.ID > maxID {
			maxID = m.ID
		}
	}
	b.nextID.Store(maxID)
}

// Add 追加一条，返回分配到的 id。
func (b *Buffer) Add(m Message) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	m.ID = b.nextID.Add(1)
	b.gen++
	b.msgs = append(b.msgs, m)
	if len(b.msgs) > b.limit {
		b.msgs = b.msgs[len(b.msgs)-b.limit:]
	}
	b.markDirtyLocked()
	return m.ID
}

// Annotate 给某条消息补上"由哪个插件处理"，用来回答"为什么机器人没反应"。
func (b *Buffer) Annotate(id int64, plugin string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.msgs) - 1; i >= 0; i-- {
		if b.msgs[i].ID == id {
			b.msgs[i].Plugin = plugin
			b.gen++ // 注释不产生新 id，代数必须自增才会落盘
			b.markDirtyLocked()
			return
		}
	}
}

// markDirtyLocked 标脏并排一个节流落盘。
// 固定 1 秒定时器，不随后续写入顺延：持续刷屏时也保证每秒至多一次
// 全量写，且任意时刻崩溃的丢失上限都是 1 秒。
func (b *Buffer) markDirtyLocked() {
	b.dirty = true
	if b.timer == nil {
		b.timer = time.AfterFunc(saveDelay, b.flush)
	}
}

// flush 把脏数据落盘。序列化在锁内拿快照，写盘在锁外（fileMu 串行）。
func (b *Buffer) flush() {
	b.mu.Lock()
	if !b.dirty || b.path == "" {
		b.timer = nil
		b.mu.Unlock()
		return
	}
	raw, gen := b.snapshotLocked()
	b.dirty = false
	b.timer = nil
	b.mu.Unlock()
	if raw == nil {
		return
	}
	b.writeFile(raw, gen)
}

// Flush 同步落盘（进程退出前调用，别丢最后一秒）。
func (b *Buffer) Flush() {
	b.mu.Lock()
	if !b.dirty || b.path == "" {
		b.mu.Unlock()
		return
	}
	raw, gen := b.snapshotLocked()
	b.dirty = false
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()
	if raw == nil {
		return
	}
	b.writeFile(raw, gen)
}

// snapshotLocked 序列化当前消息（含变更代数）；空缓冲返回 nil。
func (b *Buffer) snapshotLocked() ([]byte, int64) {
	if len(b.msgs) == 0 {
		return nil, 0
	}
	raw, err := json.Marshal(b.msgs)
	if err != nil {
		return nil, 0
	}
	return raw, b.gen
}

// writeFile 串行化写盘：旧代数（gen <= savedGen）直接跳过，
// 保证磁盘上的内容永远不比内存里的旧。
func (b *Buffer) writeFile(raw []byte, gen int64) {
	if gen <= b.savedGen.Load() {
		return
	}
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	if gen <= b.savedGen.Load() { // 排到锁后面的可能已经不是最新快照了
		return
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return
	}
	b.savedGen.Store(gen)
}

// Since 返回 id 大于 after 的消息，最多 limit 条，时间正序。
func (b *Buffer) Since(after int64, limit int) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	if limit <= 0 || limit > 500 {
		limit = 200
	}
	out := make([]Message, 0, limit)
	for i := len(b.msgs) - 1; i >= 0 && len(out) < limit; i-- {
		if b.msgs[i].ID <= after {
			break
		}
		out = append(out, b.msgs[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Recent 返回最近有消息的会话（每个会话取最后一条），按时间倒序。
func (b *Buffer) Recent(limit int) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	if limit <= 0 || limit > 100 {
		limit = 20
	}
	seen := make(map[string]bool)
	out := make([]Message, 0, limit)
	for i := len(b.msgs) - 1; i >= 0 && len(out) < limit; i-- {
		m := b.msgs[i]
		// key 要带账号：同群号在不同账号下是两个独立会话，
		// 不带会把两个账号的对话挤成一条、归属乱跳
		key := m.Account + "|" + m.ChatType + "|" + itoa(m.ChatID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

// Chats 返回出现过的会话（去重，最近活跃的在前）。
func (b *Buffer) Chats() []Message {
	return b.Recent(50)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
