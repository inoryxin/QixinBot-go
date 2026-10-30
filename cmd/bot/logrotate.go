package main

// data/bot.log 的按大小轮转。之前是 O_APPEND 永远追加，
// 热群跑一个月日志能把 5GB 小盘吃穿。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// logMaxBytes / logKeepHistory 是轮转阈值。
const (
	logMaxBytes    = 8 << 20 // 单文件 8 MB
	logKeepHistory = 4       // 保留 4 份历史 + 当前 1 份
)

// sizeRotateWriter 追加写日志文件，超过 logMaxBytes 换新文件。
// 历史文件名 <name>-<时间戳>.log，只保留最近 logKeepHistory 份。
type sizeRotateWriter struct {
	mu    sync.Mutex
	path  string
	f     *os.File
	wsize int64
}

func newSizeRotateWriter(path string) *sizeRotateWriter {
	w := &sizeRotateWriter{path: path}
	if st, err := os.Stat(path); err == nil && st.Size() >= logMaxBytes {
		// 启动即超限：先把旧文件改名让位（只改名不开新文件，
		// 构造函数末尾统一开一次，避免双开泄漏 fd）
		_ = w.rotateFile()
	}
	_ = w.reopen()
	return w
}

func (w *sizeRotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		// 上次轮转/启动时打不开文件：每条日志重试一次，恢复后自动继续写；
		// 仍然失败就丢弃本条（newLogger 的 stderr 还有输出）。
		if err := w.reopen(); err != nil {
			return len(p), nil
		}
	}
	if w.wsize+int64(len(p)) > logMaxBytes {
		if err := w.rotateLocked(); err != nil {
			// 改名失败（常见：文件被查看器占用）。fd 已重开回原文件，
			// wsize 没动 → 下一条日志会再次尝试轮转，不会永不触发。
			// 这一单条照常写进原文件，只是没轮转。
		}
	}
	if w.f == nil {
		// 轮转后重开失败（磁盘满/目录权限/安全软件锁住）会走到这里：
		// 丢这一条日志，绝不能 nil 解引用把 bot 带崩。
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.wsize += int64(n)
	return n, err
}

// rotateLocked 关旧 → 改名留历史 → 清超限 → 开新。mu 必须已持有。
// 改名失败返回错误，调用方负责重开原文件继续写。
func (w *sizeRotateWriter) rotateLocked() error {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	if err := w.rotateFile(); err != nil {
		_ = w.reopen()
		return err
	}
	_ = w.reopen()
	w.wsize = 0
	return nil
}

// rotateFile 只做改名 + 清超限历史，不动 w.f。
func (w *sizeRotateWriter) rotateFile() error {
	ext := filepath.Ext(w.path)
	stamp := time.Now().Format("20060102-150405")
	dst := strings.TrimSuffix(w.path, ext) + "-" + stamp + ext
	if err := os.Rename(w.path, dst); err != nil {
		return err
	}
	w.prune()
	return nil
}

// reopen 打开（重新打开）日志文件，并把 wsize 对齐到真实大小。
// 失败时把 f 置 nil；Write 下一条会重试，不能对 nil 取 Write。
func (w *sizeRotateWriter) reopen() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		w.f = nil
		return err
	}
	w.f = f
	if st, err := os.Stat(w.path); err == nil {
		w.wsize = st.Size()
	}
	return nil
}

// prune 删掉超出保留份数的最旧历史。
func (w *sizeRotateWriter) prune() {
	base := strings.TrimSuffix(filepath.Base(w.path), filepath.Ext(w.path))
	ext := filepath.Ext(w.path)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(w.path), base+"-*"+ext))
	if len(matches) <= logKeepHistory {
		return
	}
	sort.Strings(matches) // 时间戳文件名，字典序即时间序
	for _, old := range matches[:len(matches)-logKeepHistory] {
		_ = os.Remove(old)
	}
}

// Close 落盘前关文件。nil 接收者安全（[log] file 未配置时是 nil）。
func (w *sizeRotateWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
