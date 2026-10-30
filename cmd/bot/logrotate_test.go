package main

// 回归测试：轮转时改名/重开都失败，Write 不能对 nil 取 Write。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSizeRotateWriterWriteAfterReopenFailureDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.log")
	f, err := os.Create(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	w := &sizeRotateWriter{
		// 父目录不存在：rename 和 reopen 都会失败
		path:  filepath.Join(dir, "missing", "bot.log"),
		f:     f,
		wsize: logMaxBytes,
	}
	n, err := w.Write([]byte("x"))
	if err != nil {
		t.Fatalf("写日志不应返回错误（允许丢弃）: %v", err)
	}
	if n != 1 {
		t.Fatalf("应报告写入长度 1，得到 %d", n)
	}
}

func TestSizeRotateWriterNilClose(t *testing.T) {
	var w *sizeRotateWriter
	if err := w.Close(); err != nil {
		t.Fatalf("nil writer Close 应安全: %v", err)
	}
}
