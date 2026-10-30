package chatbuf

// 回归测试：flush（定时器）与 Flush（关机）并发落盘时，
// 磁盘上的快照不允许比内存里的旧。
// 之前两路都在锁外写同一个 .tmp 再 rename，乱序时旧快照会覆盖新快照。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func readDisk(t *testing.T, path string) []Message {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []Message
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("磁盘上的 chat.json 损坏: %v", err)
	}
	return onDisk
}

func TestConcurrentFlushNeverLosesNewerSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	b := New(100, path)

	for i := 0; i < 20; i++ {
		b.Add(Message{TS: int64(i), Text: "msg"})
	}

	// 模拟争抢：flush（旧快照）与 Flush（新快照）交错执行多轮
	var wg sync.WaitGroup
	for round := 0; round < 50; round++ {
		b.Add(Message{TS: int64(100 + round), Text: "later"})
		wg.Add(2)
		go func() { defer wg.Done(); b.flush() }()
		go func() { defer wg.Done(); b.Flush() }()
	}
	wg.Wait()
	b.Flush()

	onDisk := readDisk(t, path)
	if len(onDisk) == 0 {
		t.Fatal("磁盘上没有任何消息")
	}
	// 磁盘上的最后一条必须等于内存的最后一条（消息数 70 < 上限 100，
	// 没有环形截断，磁盘不允许落后于内存）
	b.mu.Lock()
	memMax := b.msgs[len(b.msgs)-1].ID
	b.mu.Unlock()
	if onDisk[len(onDisk)-1].ID != memMax {
		t.Fatalf("磁盘快照(%d)落后于内存(%d)", onDisk[len(onDisk)-1].ID, memMax)
	}
}

func TestStaleSnapshotSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	b := New(100, path)

	b.Add(Message{Text: "a"})
	b.Add(Message{Text: "b"})
	b.Flush() // 落盘 {a,b}

	// 旧代数的快照直接跳过，不许覆盖
	oldRaw, _ := json.Marshal([]Message{{ID: 1, Text: "a"}})
	b.mu.Lock()
	gen := b.gen - 1 // 上一代
	b.mu.Unlock()
	b.writeFile(oldRaw, gen)

	onDisk := readDisk(t, path)
	if len(onDisk) != 2 {
		t.Fatalf("旧快照不该覆盖新快照，磁盘上有 %d 条", len(onDisk))
	}
}

// 注释（Annotate）不产生新 id，但必须落盘：
// 之前按"最大消息 id"判新旧，注释完的最后一条消息永远被当旧快照跳过，
// 重启后 plugin 标签会丢。
func TestAnnotationPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	b := New(100, path)

	id1 := b.Add(Message{Text: "a"})
	id2 := b.Add(Message{Text: "b"})
	b.Flush() // 落盘 {a,b}，此时 b 还没有 plugin 标签

	b.Annotate(id2, "ai-chat") // 只改字段，不产生新 id
	b.flush()                  // 定时器路径也要能把这个变更写出去

	onDisk := readDisk(t, path)
	var found bool
	for _, m := range onDisk {
		if m.ID == id2 {
			found = true
			if m.Plugin != "ai-chat" {
				t.Fatalf("注释没有落盘，磁盘上 plugin=%q", m.Plugin)
			}
		}
	}
	if !found {
		t.Fatal("磁盘上找不到被注释的消息")
	}
	_ = id1
}
