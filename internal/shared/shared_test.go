package shared

import (
	"path/filepath"
	"testing"

	"github.com/xin/onebot/internal/storage"
)

func TestSharedPersistsToBackend(t *testing.T) {
	db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := NewWithBackend(db, "")
	s.Register("ai-persona", "current_group", "当前群人设", "群聊")
	if !s.Set("ai-persona", "ai-persona.current_group", "猫娘") {
		t.Fatal("Set 应该成功")
	}

	// 模拟重启：同一 backend 重新加载
	s2 := NewWithBackend(db, "")
	got, ok := s2.Get("ai-persona.current_group")
	if !ok || got != "猫娘" {
		t.Fatalf("重启后应该保持猫娘，实际 ok=%v value=%v", ok, got)
	}

	s2.Unregister("ai-persona")
	s3 := NewWithBackend(db, "")
	if _, ok := s3.Get("ai-persona.current_group"); ok {
		t.Fatal("删除插件后共享数据应清掉")
	}
}
