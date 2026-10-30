// Package logbuf 在内存里保留最近若干条日志，供 WebUI 实时查看。
package logbuf

import (
	"context"
	"log/slog"
	"sync"
)

// Entry 是一条日志。
type Entry struct {
	Seq   int64  `json:"seq"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Attrs string `json:"attrs,omitempty"`
}

// Buffer 是定长环形缓冲。
type Buffer struct {
	mu    sync.Mutex
	items []Entry
	max   int
	seq   int64
}

// New 创建缓冲，max 是保留条数。
func New(max int) *Buffer {
	if max <= 0 {
		max = 1000
	}
	return &Buffer{max: max, items: make([]Entry, 0, max)}
}

// Add 追加一条；超过容量时丢最旧的。
func (b *Buffer) Add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	b.items = append(b.items, e)
	if len(b.items) > b.max {
		drop := len(b.items) - b.max
		b.items = append([]Entry(nil), b.items[drop:]...)
	}
}

// Since 返回 seq > after 的条目，最多 limit 条。
func (b *Buffer) Since(after int64, limit int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	// after <= 0 表示"给我最近的"：返回尾部 limit 条。
	// 之前返回最旧的 200 条，页面上永远看到启动时的旧日志。
	if after <= 0 {
		start := len(b.items) - limit
		if start < 0 {
			start = 0
		}
		return append([]Entry(nil), b.items[start:]...)
	}

	out := make([]Entry, 0, limit)
	for _, e := range b.items {
		if e.Seq <= after {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// LastSeq 返回当前最大序号。
func (b *Buffer) LastSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Wrap 包装一个 slog.Handler，让它同时写进 buffer。
func Wrap(next slog.Handler, buf *Buffer) slog.Handler {
	return &handler{next: next, buf: buf}
}

type handler struct {
	next   slog.Handler
	buf    *Buffer
	attrs  []slog.Attr
	groups []string
}

// Enabled 永远返回 true：内存缓冲想记录**所有**级别（控制台日志页要能看 debug）。
//
// 文件输出的级别过滤在 Handle 里做（看 next.Enabled）——
// 这样"buffer 全记、文件按 config"两个需求都能满足。
func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return true
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]string, 0, 4)
	for _, a := range h.attrs {
		attrs = append(attrs, a.Key+"="+a.Value.String())
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a.Key+"="+a.Value.String())
		return true
	})
	h.buf.Add(Entry{
		Time:  r.Time.Format("15:04:05"),
		Level: r.Level.String(),
		Msg:   r.Message,
		Attrs: joinAttrs(attrs),
	})
	// **转发前必须检查内层级别**。
	// slog 的级别过滤在 Enabled 里，而 Handle 不做过滤——
	// 直接调 next.Handle 会让 config 的 level 形同虚设
	// （写 info 却把 debug 全写进文件）。
	if h.next != nil {
		if !h.next.Enabled(ctx, r.Level) {
			return nil
		}
		return h.next.Handle(ctx, r)
	}
	return nil
}

func joinAttrs(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	merged := append(append([]slog.Attr{}, h.attrs...), as...)
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithAttrs(as)
	}
	return &handler{next: next, buf: h.buf, attrs: merged, groups: h.groups}
}

func (h *handler) WithGroup(name string) slog.Handler {
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithGroup(name)
	}
	return &handler{next: next, buf: h.buf, attrs: h.attrs, groups: append(h.groups, name)}
}
