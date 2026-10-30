package plugin

import (
	"testing"
	"time"

	"github.com/xin/onebot/internal/session"
)

// 冷却最容易出错的三种情况：维度搞混、静默跳过的边界、不再冷却后能恢复。

func grpSess(uid, gid int64) *session.Session {
	return &session.Session{UserID: uid, ChatID: gid, ChatType: "group"}
}

func TestCooldownPerUser(t *testing.T) {
	c := newCooldown()
	cfg := CooldownConfig{PerUser: time.Hour}
	s := grpSess(1, 100)

	if ok, _ := c.Allow("p", s, cfg); !ok {
		t.Fatal("第一次应该放行")
	}
	if ok, wait := c.Allow("p", s, cfg); ok {
		t.Fatal("同一用户第二次应该被拦")
	} else if wait <= 0 {
		t.Fatalf("应该返回剩余时间，实际 %v", wait)
	}
	// 换个用户应该放行
	if ok, _ := c.Allow("p", grpSess(2, 100), cfg); !ok {
		t.Fatal("不同用户不该互相影响")
	}
}

func TestCooldownPerGroup(t *testing.T) {
	c := newCooldown()
	cfg := CooldownConfig{PerGroup: time.Hour}

	if ok, _ := c.Allow("p", grpSess(1, 100), cfg); !ok {
		t.Fatal("第一次应该放行")
	}
	// 同群不同人 → 拦
	if ok, _ := c.Allow("p", grpSess(2, 100), cfg); ok {
		t.Fatal("同群第二次应该被拦")
	}
	// 不同群 → 放行
	if ok, _ := c.Allow("p", grpSess(3, 200), cfg); !ok {
		t.Fatal("不同群不该互相影响")
	}
}

func TestCooldownPerPlugin(t *testing.T) {
	c := newCooldown()
	cfg := CooldownConfig{PerPlugin: time.Hour}

	if ok, _ := c.Allow("p", grpSess(1, 100), cfg); !ok {
		t.Fatal("第一次应该放行")
	}
	// 完全不同的用户和群，但插件级冷却 → 拦
	if ok, _ := c.Allow("p", grpSess(99, 999), cfg); ok {
		t.Fatal("插件级冷却应该拦住所有")
	}
	// 别的插件不受影响
	if ok, _ := c.Allow("other", grpSess(1, 100), cfg); !ok {
		t.Fatal("不同插件不该互相影响")
	}
}

func TestCooldownDisabled(t *testing.T) {
	c := newCooldown()
	// 全 0 = 不限制
	for i := 0; i < 10; i++ {
		if ok, _ := c.Allow("p", grpSess(1, 100), CooldownConfig{}); !ok {
			t.Fatal("没配冷却时不该拦")
		}
	}
}

func TestCooldownExpires(t *testing.T) {
	c := newCooldown()
	cfg := CooldownConfig{PerUser: 10 * time.Millisecond}
	s := grpSess(1, 100)

	if ok, _ := c.Allow("p", s, cfg); !ok {
		t.Fatal("第一次应该放行")
	}
	if ok, _ := c.Allow("p", s, cfg); ok {
		t.Fatal("冷却中应该拦")
	}
	time.Sleep(15 * time.Millisecond)
	if ok, _ := c.Allow("p", s, cfg); !ok {
		t.Fatal("冷却过了应该放行")
	}
}

func TestCooldownRejectedDoesNotResetTimer(t *testing.T) {
	// 被拦的那次不该刷新计时，否则一直刷就永远冷却
	c := newCooldown()
	cfg := CooldownConfig{PerUser: 30 * time.Millisecond}
	s := grpSess(1, 100)

	c.Allow("p", s, cfg) // 第一次放行
	for i := 0; i < 5; i++ {
		time.Sleep(5 * time.Millisecond)
		c.Allow("p", s, cfg) // 一直被拦，但不该刷新时间
	}
	time.Sleep(10 * time.Millisecond)
	// 距第一次已经 35ms > 30ms，应该放行
	if ok, _ := c.Allow("p", s, cfg); !ok {
		t.Fatal("被拦的调用不该刷新计时")
	}
}

func TestCooldownConfigParsing(t *testing.T) {
	cfg := CooldownSpec{PerUser: "5s", PerGroup: "1m", PerPlugin: "bad"}.toConfig()
	if cfg.PerUser != 5*time.Second {
		t.Fatalf("per_user 解析错: %v", cfg.PerUser)
	}
	if cfg.PerGroup != time.Minute {
		t.Fatalf("per_group 解析错: %v", cfg.PerGroup)
	}
	if cfg.PerPlugin != 0 {
		t.Fatalf("非法值应该按 0 处理，实际 %v", cfg.PerPlugin)
	}
}
