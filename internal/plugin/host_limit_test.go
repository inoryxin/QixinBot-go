package plugin

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadLimitedLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("hello\nworld\n"), 8)
	line, tooLong, err := readLimitedLine(r, 64)
	if err != nil || tooLong || string(line) != "hello\n" {
		t.Fatalf("正常行读取失败: line=%q tooLong=%v err=%v", line, tooLong, err)
	}

	long := strings.Repeat("x", 300) + "\nnext\n"
	r2 := bufio.NewReaderSize(strings.NewReader(long), 16)
	_, tooLong, err = readLimitedLine(r2, 64)
	if err != nil {
		t.Fatalf("超长行不应报错: %v", err)
	}
	if !tooLong {
		t.Fatal("超长行应标记 tooLong")
	}
	// 超长行被丢弃后，下一行仍应正常读出
	line, tooLong, err = readLimitedLine(r2, 64)
	if err != nil || tooLong || string(line) != "next\n" {
		t.Fatalf("丢弃后应能继续读: line=%q tooLong=%v err=%v", line, tooLong, err)
	}

	// 边界：内容恰好等于 max 的行是合法的，不能当超长丢掉
	exact := strings.Repeat("y", 64) + "\n"
	r3 := bufio.NewReaderSize(strings.NewReader(exact), 8)
	line, tooLong, err = readLimitedLine(r3, 64)
	if err != nil || tooLong || string(line) != exact {
		t.Fatalf("恰好 max 的行应正常读出: line=%d字节 tooLong=%v err=%v", len(line), tooLong, err)
	}
	// 只超 1 字节就该判超长
	over := strings.Repeat("z", 65) + "\n"
	r4 := bufio.NewReaderSize(strings.NewReader(over), 8)
	_, tooLong, err = readLimitedLine(r4, 64)
	if err != nil || !tooLong {
		t.Fatalf("超 1 字节应判超长: tooLong=%v err=%v", tooLong, err)
	}
}
