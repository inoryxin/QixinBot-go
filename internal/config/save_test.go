package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 并发保存不能互相截断临时文件，也不能报 rename 冲突。
func TestSaveConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &Config{}
			c.Web.Username = "admin"
			errs <- Save(path, c)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发保存失败: %v", err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatalf("配置文件没有正确写出: %v", err)
	}
}
