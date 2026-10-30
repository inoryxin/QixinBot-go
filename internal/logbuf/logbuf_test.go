package logbuf

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// 这个回归的背景：Wrap 的 Handle 直接调 next.Handle，
// 绕过了内层的级别过滤，导致 config 写 level="info" 时
// debug 仍然写进文件（也写进 buffer）。

func TestWrapRespectsInnerLevel(t *testing.T) {
	var file bytes.Buffer
	buf := New(100)

	// 内层只允许 info 以上
	inner := slog.NewTextHandler(&file, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(Wrap(inner, buf))

	log.Debug("这条不该进文件")
	log.Info("这条该进文件")

	out := file.String()
	if strings.Contains(out, "这条不该进文件") {
		t.Fatalf("debug 不该写进文件: %s", out)
	}
	if !strings.Contains(out, "这条该进文件") {
		t.Fatalf("info 应该写进文件: %s", out)
	}
}

func TestWrapBufferRecordsAllLevels(t *testing.T) {
	// buffer 想记所有级别（控制台日志页要能看 debug）
	var file bytes.Buffer
	buf := New(100)

	inner := slog.NewTextHandler(&file, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(Wrap(inner, buf))

	log.Debug("debug 也要进 buffer")
	log.Info("info 也进")

	entries := buf.Since(0, 10)
	hasDebug := false
	for _, e := range entries {
		if e.Level == "DEBUG" {
			hasDebug = true
		}
	}
	if !hasDebug {
		t.Fatal("buffer 应该记录 DEBUG（控制台要能看）")
	}
}

func TestWrapNilNext(t *testing.T) {
	// next 为 nil 时不该 panic
	buf := New(10)
	log := slog.New(Wrap(nil, buf))
	log.Info("只进 buffer")
	if len(buf.Since(0, 10)) == 0 {
		t.Fatal("应该记录了")
	}
}

func TestWrapWarnLevel(t *testing.T) {
	var file bytes.Buffer
	buf := New(100)

	inner := slog.NewTextHandler(&file, &slog.HandlerOptions{Level: slog.LevelWarn})
	log := slog.New(Wrap(inner, buf))

	log.Info("info 不该进文件")
	log.Warn("warn 该进")

	out := file.String()
	if strings.Contains(out, "info 不该进文件") {
		t.Fatalf("info 不该写进 warn 级别: %s", out)
	}
	if !strings.Contains(out, "warn 该进") {
		t.Fatalf("warn 应该写进: %s", out)
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
