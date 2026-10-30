package storage

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func tmpPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// 两个后端必须行为一致，所以同一套断言跑两遍。
func testBackend(t *testing.T, b Backend) {
	t.Helper()

	// 不存在返回 false
	if _, ok, err := b.Get("b1", "missing"); err != nil || ok {
		t.Fatalf("未命中的 key 应该返回 (nil,false,nil)，实际 ok=%v err=%v", ok, err)
	}

	// 写入再读回
	if err := b.Set("b1", "k1", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	v, ok, err := b.Get("b1", "k1")
	if err != nil || !ok {
		t.Fatalf("应该读得到: ok=%v err=%v", ok, err)
	}
	var got map[string]int
	if err := json.Unmarshal(v, &got); err != nil || got["v"] != 1 {
		t.Fatalf("值不对: %s", v)
	}

	// 覆盖
	if err := b.Set("b1", "k1", []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	v, _, _ = b.Get("b1", "k1")
	if string(v) != `{"v":2}` {
		t.Fatalf("覆盖失败: %s", v)
	}

	// 不同 bucket 互不干扰
	if err := b.Set("b2", "k1", []byte(`"other"`)); err != nil {
		t.Fatal(err)
	}
	v, _, _ = b.Get("b1", "k1")
	if string(v) != `{"v":2}` {
		t.Fatal("bucket 隔离失败")
	}

	// List 只列本 bucket
	items, err := b.List("b1")
	if err != nil || len(items) != 1 {
		t.Fatalf("List 应该只有 1 条，实际 %d 条 (err=%v)", len(items), err)
	}

	// 删除
	if err := b.Delete("b1", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.Get("b1", "k1"); ok {
		t.Fatal("删除后还能读到")
	}
}

func TestSQLiteBackend(t *testing.T) {
	db, err := OpenSQLite(tmpPath(t, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	testBackend(t, db)
}

func TestJSONBackend(t *testing.T) {
	j, err := OpenJSON(tmpPath(t, "test.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	testBackend(t, j)
}

func TestJSONPersists(t *testing.T) {
	path := tmpPath(t, "persist.json")
	j, _ := OpenJSON(path)
	_ = j.Set("b", "k", []byte(`"hello"`))
	j.Close()

	// 重新打开应该还能读到
	j2, err := OpenJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, _ := j2.Get("b", "k")
	if !ok || string(v) != `"hello"` {
		t.Fatalf("重启后数据丢了: %s", v)
	}
}

func TestSQLitePersists(t *testing.T) {
	path := tmpPath(t, "persist.db")
	db, _ := OpenSQLite(path)
	_ = db.Set("b", "k", []byte(`"hello"`))
	db.Close()

	db2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	v, ok, _ := db2.Get("b", "k")
	if !ok || string(v) != `"hello"` {
		t.Fatalf("重启后数据丢了: %s", v)
	}
}

func TestMigrateJSONToSQLite(t *testing.T) {
	jsonPath := tmpPath(t, "migrate.json")
	j, _ := OpenJSON(jsonPath)
	_ = j.Set("shared", "a", []byte(`1`))
	_ = j.Set("shared", "b", []byte(`2`))
	_ = j.Set("settings", "c", []byte(`3`))
	j.Close()

	j2, _ := OpenJSON(jsonPath) // 重新读一遍，模拟从磁盘加载
	db, _ := OpenSQLite(tmpPath(t, "migrate.db"))
	defer db.Close()

	moved, err := Migrate(j2, db, []string{"shared", "settings"})
	if err != nil {
		t.Fatal(err)
	}
	if moved != 3 {
		t.Fatalf("应该迁移 3 条，实际 %d", moved)
	}

	// 迁移后 SQLite 里应该有
	items, _ := db.List("shared")
	if len(items) != 2 {
		t.Fatalf("shared 应该有 2 条，实际 %d", len(items))
	}
	if string(items["a"]) != "1" {
		t.Fatalf("值不对: %s", items["a"])
	}
}

func TestMigrateSkipsNonEmptyBucket(t *testing.T) {
	// 目标已有数据时不该覆盖（防止二次迁移把新数据冲掉）
	j, _ := OpenJSON(tmpPath(t, "s.json"))
	_ = j.Set("shared", "old", []byte(`"old"`))

	db, _ := OpenSQLite(tmpPath(t, "s.db"))
	defer db.Close()
	_ = db.Set("shared", "new", []byte(`"new"`))

	moved, err := Migrate(j, db, []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Fatalf("目标非空时不该迁移，实际搬了 %d 条", moved)
	}
	if _, ok, _ := db.Get("shared", "old"); ok {
		t.Fatal("不该把旧数据塞进去")
	}
}
