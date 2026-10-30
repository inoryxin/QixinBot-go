package plugin

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// db.sqlite 内置能力的三种 op。这组测试守着一个真实踩过的坑：
// 插件里写 db('get', ...) 按主键查一行，但内置能力只支持 exec / all，
// 结果运行时才炸（"未知 op get"），消息干脆没回复。

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLiteExecOp(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	res, err := runSQLiteQuery(ctx, db, map[string]any{
		"op":  "exec",
		"sql": "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)",
	})
	if err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	_ = res

	res, err = runSQLiteQuery(ctx, db, map[string]any{
		"op":     "exec",
		"sql":    "INSERT INTO t (name) VALUES (?)",
		"params": []any{"小明"},
	})
	if err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	m := res.(map[string]any)
	if m["affected"] != int64(1) {
		t.Fatalf("affected 应为 1，实际 %v", m["affected"])
	}
	if m["last_id"] != int64(1) {
		t.Fatalf("last_id 应为 1，实际 %v", m["last_id"])
	}
}

func TestSQLiteAllOp(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _ = runSQLiteQuery(ctx, db, map[string]any{
		"op": "exec", "sql": "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)",
	})
	for _, n := range []string{"a", "b"} {
		_, _ = runSQLiteQuery(ctx, db, map[string]any{
			"op": "exec", "sql": "INSERT INTO t (name) VALUES (?)", "params": []any{n},
		})
	}

	res, err := runSQLiteQuery(ctx, db, map[string]any{"op": "all", "sql": "SELECT * FROM t ORDER BY id"})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	m := res.(map[string]any)
	rows, _ := m["rows"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("应返回 2 行，实际 %d", len(rows))
	}
	if rows[0]["name"] != "a" {
		t.Fatalf("第一行 name 应为 a，实际 %v", rows[0]["name"])
	}
}

func TestSQLiteGetOpReturnsFirstRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _ = runSQLiteQuery(ctx, db, map[string]any{
		"op": "exec", "sql": "CREATE TABLE persona (name TEXT PRIMARY KEY, content TEXT)",
	})
	_, _ = runSQLiteQuery(ctx, db, map[string]any{
		"op": "exec", "sql": "INSERT INTO persona VALUES (?, ?)", "params": []any{"私聊", "温柔"},
	})

	res, err := runSQLiteQuery(ctx, db, map[string]any{
		"op": "get", "sql": "SELECT name, content FROM persona WHERE name = ?", "params": []any{"私聊"},
	})
	if err != nil {
		t.Fatalf("get 失败: %v", err)
	}
	m := res.(map[string]any)
	row, ok := m["row"].(map[string]any)
	if !ok {
		t.Fatalf("应返回 row 对象，实际 %#v", m["row"])
	}
	if row["content"] != "温柔" {
		t.Fatalf("content 应为 温柔，实际 %v", row["content"])
	}
}

func TestSQLiteGetOpMissingReturnsNull(t *testing.T) {
	// 查不到不是错误：插件里 getPersona 拿 null 才会走默认人设
	db := newTestDB(t)
	ctx := context.Background()
	_, _ = runSQLiteQuery(ctx, db, map[string]any{
		"op": "exec", "sql": "CREATE TABLE persona (name TEXT PRIMARY KEY)",
	})

	res, err := runSQLiteQuery(ctx, db, map[string]any{
		"op": "get", "sql": "SELECT * FROM persona WHERE name = ?", "params": []any{"没有"},
	})
	if err != nil {
		t.Fatalf("查不到不该报错: %v", err)
	}
	m := res.(map[string]any)
	if m["row"] != nil {
		t.Fatalf("查不到应返回 null，实际 %#v", m["row"])
	}
}

func TestSQLiteUnknownOp(t *testing.T) {
	db := newTestDB(t)
	_, err := runSQLiteQuery(context.Background(), db, map[string]any{
		"op": "delete", "sql": "SELECT 1",
	})
	if err == nil {
		t.Fatal("未知 op 应该报错")
	}
}
