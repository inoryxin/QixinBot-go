// Package audit 记录"谁在什么时候对谁做了什么"，按天写 JSONL。
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry 是一条审计记录。
type Entry struct {
	TS       int64  `json:"ts"`
	Account  string `json:"account,omitempty"`
	ChatType string `json:"chat_type,omitempty"`
	ChatID   int64  `json:"chat_id,omitempty"`
	UserID   int64  `json:"user_id,omitempty"`
	Plugin   string `json:"plugin,omitempty"`
	Action   string `json:"action"` // command | send | tool | onebot
	Detail   string `json:"detail,omitempty"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
}

// retentionDays 审计文件保留天数。文档说"保留最近两天"之前从未实现过
// 清理，文件永远增长；这里统一按 7 天滚动删除。
const retentionDays = 7

// Logger 按天追加写文件，保留最近若干天。
type Logger struct {
	dir string
	mu  sync.Mutex
}

// New 创建审计日志；dir 不存在会自动建。
func New(dir string) *Logger {
	_ = os.MkdirAll(dir, 0o755)
	l := &Logger{dir: dir}
	go l.pruneLoop()
	return l
}

// prune 清理过期日期的审计文件。
func (l *Logger) prune() {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format("2006-01-02")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(name, ".jsonl")
		if len(day) != 10 || day >= cutoff {
			continue
		}
		if err := os.Remove(filepath.Join(l.dir, name)); err != nil {
			continue
		}
	}
}

// pruneLoop 每天清一次过期审计文件。
func (l *Logger) pruneLoop() {
	l.prune()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for range t.C {
		l.prune()
	}
}

func (l *Logger) file(day string) string {
	return filepath.Join(l.dir, day+".jsonl")
}

// Write 追加一条记录。
func (l *Logger) Write(e Entry) {
	if l == nil {
		return
	}
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	raw = append(raw, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	day := time.Unix(e.TS, 0).Format("2006-01-02")
	f, err := os.OpenFile(l.file(day), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(raw)
}

// Recent 返回最近 limit 条（从今天和昨天合并，新的在前）。
func (l *Logger) Recent(limit int) []Entry {
	if l == nil {
		return nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	days := []string{
		time.Now().Format("2006-01-02"),
		time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
	}

	var out []Entry
	for _, day := range days {
		entries := l.readDay(day)
		// 从后往前取，凑够 limit
		for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, entries[i])
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (l *Logger) readDay(day string) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	raw, err := os.ReadFile(l.file(day))
	if err != nil {
		return nil
	}
	var out []Entry
	for _, line := range splitLines(raw) {
		var e Entry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

// Day 返回今天的文件名（给 WebUI 显示）。
func (l *Logger) Day() string { return time.Now().Format("2006-01-02") }
