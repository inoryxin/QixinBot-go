package plugin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 并发限流的关键：不超限、不饿死、超时能退。

func TestLimiterAllowsUpToLimit(t *testing.T) {
	l := newCapLimiter()
	ctx := context.Background()

	// 拿满 3 个
	for i := 0; i < 3; i++ {
		release, err := l.Acquire(ctx, "p", "cap", 3)
		if err != nil {
			t.Fatalf("第 %d 个应该拿到: %v", i+1, err)
		}
		defer release()
	}
	if got := l.InFlight("p", "cap"); got != 3 {
		t.Fatalf("应该有 3 个在跑，实际 %d", got)
	}
}

func TestLimiterBlocksWhenFull(t *testing.T) {
	l := newCapLimiter()
	release, _ := l.Acquire(context.Background(), "p", "cap", 1)
	defer release()

	// 第 2 个应该拿不到（短超时）
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := l.Acquire(ctx, "p", "cap", 1); err == nil {
		t.Fatal("满了应该报错")
	}
}

func TestLimiterReleasesSlot(t *testing.T) {
	l := newCapLimiter()
	release, _ := l.Acquire(context.Background(), "p", "cap", 1)

	release()

	// 释放后应该能再拿
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	release2, err := l.Acquire(ctx, "p", "cap", 1)
	if err != nil {
		t.Fatalf("释放后应该能拿: %v", err)
	}
	release2()
}

func TestLimiterReleaseIsIdempotent(t *testing.T) {
	// 重复 release 不能把信号量搞乱（会 panic 或负数）
	l := newCapLimiter()
	release, _ := l.Acquire(context.Background(), "p", "cap", 1)
	release()
	release()
	release()

	if got := l.InFlight("p", "cap"); got != 0 {
		t.Fatalf("应该是 0，实际 %d", got)
	}
}

func TestLimiterNoLimitWhenZero(t *testing.T) {
	l := newCapLimiter()
	// limit=0 不限
	for i := 0; i < 100; i++ {
		release, err := l.Acquire(context.Background(), "p", "cap", 0)
		if err != nil {
			t.Fatalf("不限时不该报错: %v", err)
		}
		release()
	}
}

func TestLimiterDifferentProvidersIndependent(t *testing.T) {
	l := newCapLimiter()
	// p1 拿满
	r1, _ := l.Acquire(context.Background(), "p1", "cap", 1)
	defer r1()

	// p2 不受影响
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r2, err := l.Acquire(ctx, "p2", "cap", 1)
	if err != nil {
		t.Fatalf("不同提供者不该互相影响: %v", err)
	}
	r2()
}

func TestLimiterConcurrentAcquireRespectsLimit(t *testing.T) {
	l := newCapLimiter()
	const limit = 3
	const workers = 20

	var peak atomic.Int64
	var cur atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire(context.Background(), "p", "cap", limit)
			if err != nil {
				return
			}
			n := cur.Add(1)
			// 记录峰值
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			cur.Add(-1)
			release()
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > limit {
		t.Fatalf("峰值 %d 超过了上限 %d", p, limit)
	}
}

func TestLimiterForget(t *testing.T) {
	l := newCapLimiter()
	release, _ := l.Acquire(context.Background(), "p", "cap", 1)
	defer release()

	l.Forget("p")
	if got := l.InFlight("p", "cap"); got != 0 {
		t.Fatalf("forget 后应该是 0，实际 %d", got)
	}
}
