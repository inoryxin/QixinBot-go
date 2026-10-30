package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xin/onebot/internal/session"
	"github.com/xin/onebot/internal/trace"
)

// 这轮修复的三个回归：
//   1. host.* 请求不能被 init 超时截断
//   2. 限流器按 provider:capability 区分
//   3. 失败默认不重试（避免副作用执行两次）

func TestLimiterPerCapability(t *testing.T) {
	l := newCapLimiter()
	ctx := context.Background()

	// 同一个服务的两个能力，各有各的名额
	r1, err := l.Acquire(ctx, "svc", "fast", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	// slow 只有 1 个名额，但 fast 占着不该影响它
	r2, err := l.Acquire(ctx, "svc", "slow", 1)
	if err != nil {
		t.Fatalf("不同能力不该互相影响: %v", err)
	}
	defer r2()

	if got := l.InFlight("svc", "fast"); got != 1 {
		t.Fatalf("fast 应该 1 个，实际 %d", got)
	}
	if got := l.InFlight("svc", "slow"); got != 1 {
		t.Fatalf("slow 应该 1 个，实际 %d", got)
	}
}

func TestLimiterSameCapabilitySharesQuota(t *testing.T) {
	l := newCapLimiter()
	ctx := context.Background()
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	r1, _ := l.Acquire(ctx, "svc", "cap", 1)
	defer r1()

	// 同一个能力的第二个名额应该拿不到
	if _, err := l.Acquire(shortCtx, "svc", "cap", 1); err == nil {
		t.Fatal("同一能力应该共用名额")
	}
}

func TestLimiterForgetClearsAllCapabilities(t *testing.T) {
	l := newCapLimiter()
	ctx := context.Background()

	r1, _ := l.Acquire(ctx, "svc", "a", 1)
	r2, _ := l.Acquire(ctx, "svc", "b", 1)
	defer r1()
	defer r2()

	l.Forget("svc")
	if l.InFlight("svc", "a") != 0 || l.InFlight("svc", "b") != 0 {
		t.Fatal("Forget 应该清掉这个服务的所有能力")
	}
}

func TestRetryDefaultsToSafe(t *testing.T) {
	// 默认不重试：能力可能有副作用
	spec := CapabilitySpec{Name: "x"}
	if spec.Retry == "always" {
		t.Fatal("默认不该是 always")
	}
}

func TestCallTimeoutIndependentOfInit(t *testing.T) {
	// init 是启动握手，call 是单次宿主调用的兜底；
	// 两者独立，call 默认远大于 init（否则 host.ask 的 60 秒会被 10 秒截断）
	var spec TimeoutsSpec
	if d := spec.InitTimeout(); d != 0 {
		t.Fatalf("没配时不该返回非 0: %v", d)
	}
	if d := spec.CallTimeout(); d != 0 {
		t.Fatalf("没配时不该返回非 0: %v", d)
	}

	// 配了各自生效
	spec = TimeoutsSpec{Init: "10s", Call: "30m"}
	if spec.InitTimeout().Seconds() != 10 {
		t.Fatal("init 解析错")
	}
	if spec.CallTimeout().Minutes() != 30 {
		t.Fatal("call 解析错")
	}
}

func TestMatchesDeclaredCommand(t *testing.T) {
	man := &Manifest{Commands: []CommandSpec{
		{ID: "bili.status", Usage: "bili", Aliases: []string{"b站"}},
		{ID: "bili.check", Usage: "bili check"},
	}}

	cases := []struct {
		text string
		want bool
	}{
		{"bili", true},
		{"bili check", true},
		{"b站", true},
		{"bili xxx", true}, // 前缀命中就算（参数交给插件自己判断）
		// "bilibili" 也命中：matchCommandLen 支持"中文/短命令直接跟参数"
		//（/天气北京 这种写法），所以前缀相同就算，宁严勿漏。
		{"bilibili", true},
		{"今天天气", false},
		{"", false},
	}
	for _, c := range cases {
		if got := matchesDeclaredCommand(man, c.text); got != c.want {
			t.Errorf("%q: want %v, got %v", c.text, c.want, got)
		}
	}
}

func TestFailureHintCooldown(t *testing.T) {
	// 明确的命令被冷却时，提示要说"等多久"，不能说"没有这个命令"
	tr := trace.New("/status")
	tr.Add("冷却", "status", false, "还需 3 秒")
	sess := &session.Session{
		Text:      "/status",
		HasPrefix: true,
		Trace:     tr,
	}
	got := FailureHint(sess)
	if !strings.Contains(got, "频繁") {
		t.Fatalf("冷却提示应该说太频繁，实际: %q", got)
	}
	if strings.Contains(got, "没有这个命令") {
		t.Fatalf("冷却不该说成命令不存在: %q", got)
	}
}

func TestFailureHintPermission(t *testing.T) {
	tr := trace.New("/home")
	tr.Add("权限", "home-assistant", false, "权限不足")
	sess := &session.Session{Text: "/home", HasPrefix: true, Trace: tr}
	if got := FailureHint(sess); !strings.Contains(got, "权限") {
		t.Fatalf("应该提示权限不足，实际: %q", got)
	}
}

func TestFailureHintSilentForUnknownCommand(t *testing.T) {
	// 带前缀但没有这个命令：不回（打错字不该刷屏）
	tr := trace.New("/asdf")
	tr.Add("分发", "", true, "开始")
	sess := &session.Session{Text: "asdf", HasPrefix: true, Trace: tr}
	if got := FailureHint(sess); got != "" {
		t.Fatalf("未知命令应该沉默，实际回了: %q", got)
	}
}

func TestFailureHintSilentForPlainChat(t *testing.T) {
	sess := &session.Session{Text: "今天天气", HasPrefix: false}
	if got := FailureHint(sess); got != "" {
		t.Fatalf("普通聊天不该被提示: %q", got)
	}
}
