package config

// FlexBool 回归测试：没设置过的键在 WebUI 保存配置时必须落成 true
// （生效值），而不是零值 false——否则保存任意配置都会把
// cohost_enabled 静默写成 false，下次重启共享宿主被关掉。

import (
	"testing"

	"github.com/BurntSushi/toml"
)

type flexFixture struct {
	Cohost FlexBool `toml:"cohost_enabled"`
}

func TestFlexBoolUnsetMarshalsToEffectiveValue(t *testing.T) {
	var f flexFixture // Cohost 未设置
	raw, err := toml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	want := "cohost_enabled = true"
	if !contains(string(raw), want) {
		t.Fatalf("未设置的 FlexBool 应序列化为生效值 true，得到:\n%s", raw)
	}
	// 回读：落盘的 true 必须被读回 true（Set=true, V=true）
	var back flexFixture
	if _, err := toml.Decode(string(raw), &back); err != nil {
		t.Fatal(err)
	}
	if !back.Cohost.Bool() {
		t.Fatal("回读后生效值应为 true")
	}
}

func TestFlexBoolExplicitValuesSurvive(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"cohost_enabled = false", false},
		{"cohost_enabled = true", true},
		{"", true}, // 键不存在 = 缺省 true
	} {
		var f flexFixture
		if _, err := toml.Decode(tc.in, &f); err != nil {
			t.Fatal(err)
		}
		if got := f.Cohost.Bool(); got != tc.want {
			t.Fatalf("%q: 生效值应为 %v，得到 %v", tc.in, tc.want, got)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
