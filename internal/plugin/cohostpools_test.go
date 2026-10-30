package plugin

import (
	"path/filepath"
	"testing"
)

func TestPoolStoreCreateAssignPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cohost-pools.json")
	s, err := NewPoolStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create("基础", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("echo", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("trigger", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("what2eat", p.ID); err == nil {
		t.Fatal("超过上限应拒绝")
	}
	if err := s.Delete(p.ID); err == nil {
		t.Fatal("池里还有成员时不能删除")
	}
	s2, err := NewPoolStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ass, ok := s2.PoolOf("echo")
	if !ok || got.Name != "基础" || ass.State != CohostStateShared {
		t.Fatalf("重启后归属丢失: %+v %+v %v", got, ass, ok)
	}
	if err := s2.Assign("echo", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s2.PoolOf("echo"); ok {
		t.Fatal("清空归属后应回 quarantine")
	}
}

func TestPoolEvictionDoesNotCountAsMember(t *testing.T) {
	s, err := NewPoolStore(filepath.Join(t.TempDir(), "pools.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create("p", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("a", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Evict("a"); err != nil {
		t.Fatal(err)
	}
	_, ass, ok := s.PoolOf("a")
	if !ok || ass.State != "evicted" {
		t.Fatalf("应保留逐出状态: %+v %v", ass, ok)
	}
	// 逐出成员不占名额，b 可以进
	if err := s.Assign("b", p.ID); err != nil {
		t.Fatalf("逐出成员不应占名额: %v", err)
	}
	// 手动恢复 a 前先给 b 腾位置（池上限 1）
	if err := s.Assign("b", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("a", p.ID); err != nil {
		t.Fatal(err)
	}
	_, ass, _ = s.PoolOf("a")
	if ass.State != CohostStateShared {
		t.Fatalf("恢复后应为 shared，得到 %+v", ass)
	}
}

func TestPoolUpdateAndCrashPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pools.json")
	s, err := NewPoolStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create("old", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Assign("a", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePool(p.ID, "new", 1); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RecordCrash("a"); err != nil || n != 1 {
		t.Fatalf("crash count: %d %v", n, err)
	}
	if n, _ := s.RecordCrash("a"); n != 2 {
		t.Fatalf("second crash count: %d", n)
	}
	s2, err := NewPoolStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, ass, ok := s2.PoolOf("a")
	if !ok || len(ass.CrashTimes) != 2 {
		t.Fatalf("crash times should persist: %+v %v", ass, ok)
	}
	if err := s2.ResetCrashes("a"); err != nil {
		t.Fatal(err)
	}
	if _, ass, _ = s2.PoolOf("a"); len(ass.CrashTimes) != 0 {
		t.Fatalf("reset failed: %+v", ass)
	}
}
