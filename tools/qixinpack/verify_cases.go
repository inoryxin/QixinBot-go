package main

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// verifyAskNonBlocking 验证：一个群在 host.ask 等待时，
// **另一个群**调同一个插件应该立刻响应。
//
// 这是 readLoop 并发修复的核心验证点——之前会卡住。
func verifyAskNonBlocking(c *verifyCtx) (bool, string) {
	// 1. 群 A 发起 /bind，进入等待
	if err := c.push(1001, 10001, "/bind"); err != nil {
		return false, fmt.Sprintf("推送 /bind 失败: %v", err)
	}
	// 等插件真正进入 ask（日志里出现 START）
	if _, ok := c.waitLog("VERIFY_ASK_START", 10*time.Second); !ok {
		return false, "插件没进入 ask 等待（日志里没有 VERIFY_ASK_START）"
	}

	// 2. **群 B** 调同一个插件（vping），它应该立刻被处理
	//
	// 判断依据是**验证插件自己打的专用标记**（带群号）：
	//   VERIFY_VPING 2002
	//
	// 不用框架日志（那是 Debug 级别，级别一改就失效），
	// 也不查回复内容（host.reply 发出去的东西不进日志）。
	//
	// 标记在插件里是 reply **之后**打的，
	// 所以看到它就说明整段处理走完了。
	marker := fmt.Sprintf("VERIFY_VPING %d", 2002)
	started := time.Now()
	if err := c.push(2002, 10002, "/vping"); err != nil {
		return false, fmt.Sprintf("推送 /vping 失败: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	got := false
	for time.Now().Before(deadline) {
		if c.logCount(marker) > 0 {
			got = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	elapsed := time.Since(started)

	if !got {
		return false, fmt.Sprintf("群 B 的 /vping 在 5 秒内没响应（被 ask 堵住了），耗时 %v", elapsed)
	}
	if elapsed > 3*time.Second {
		return false, fmt.Sprintf("群 B 响应太慢（%v），可能被 ask 部分阻塞", elapsed)
	}

	// 3. 回答群 A，让它结束（清理）
	_ = c.push(1001, 10001, "小明")
	if _, ok := c.waitLog("VERIFY_ASK_END", 5*time.Second); !ok {
		return false, "群 A 的 ask 没结束"
	}

	return true, fmt.Sprintf("群 B 在 %v 内响应，没有被 ask 堵住", elapsed.Round(time.Millisecond))
}

// verifyConcurrentGroups 验证：两个群同时调同一个插件，
// 配了 concurrency = "session" 时应该并行（总耗时接近单次）。
func verifyConcurrentGroups(c *verifyCtx) (bool, string) {
	// 两个群同时调同一个插件（vping 秒回）。
	// 这里验证"都处理了"；真正的并行度要靠慢插件，
	// 但那个会拖长验证时间，先用冒烟版。
	//
	// 注意：判断依据是日志里的"已处理"，不是回复内容——
	// host.reply 发出去的消息不进日志。
	if err := c.push(3001, 10001, "/vping"); err != nil {
		return false, fmt.Sprintf("群1 失败: %v", err)
	}
	if err := c.push(3002, 10002, "/vping"); err != nil {
		return false, fmt.Sprintf("群2 失败: %v", err)
	}

	m1 := "VERIFY_VPING 3001"
	m2 := "VERIFY_VPING 3002"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.logCount(m1) > 0 && c.logCount(m2) > 0 {
			return true, "两个群都处理了"
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, fmt.Sprintf("群 3001 处理 %d 次、群 3002 处理 %d 次（各应 1 次）",
		c.logCount(m1), c.logCount(m2))
}

// writeZip 把文件列表打成一个 zip。
func writeZip(outPath string, files map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			zw.Close()
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			zw.Close()
			return err
		}
	}
	return zw.Close()
}
