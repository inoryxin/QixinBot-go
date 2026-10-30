package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// 结构化命令是给插件作者的便利：声明了 args 就不用自己切字符串。
// 这些测试锁住解析规则，避免以后改动悄悄破坏插件。

func manifestWithCmd(t *testing.T, body string) *Manifest {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "test-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `id = "test-plugin"
name = "测试"
version = "1.0.0"
apiVersion = "1.x"
exec = ["node", "app/index.js"]
description = "测试"

[[commands]]
id = "weather.current"
usage = "weather <city>"
description = "查天气"
args = { city = { type = "string", required = true, help = "城市名" } }

[[commands]]
id = "echo.say"
usage = "echo <content>"
args = { content = { type = "rest", required = true } }

[[commands]]
id = "test.ping"
usage = "ping"
aliases = ["p", "乒乓"]

[runtime]
kind = "node"
`
	_ = body
	path := filepath.Join(dir, "manifest.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("加载清单失败: %v", err)
	}
	return man
}

func TestParseCommandSingleArg(t *testing.T) {
	man := manifestWithCmd(t, "")

	m := ParseCommand(man, "weather 上海")
	if m == nil {
		t.Fatal("应该匹配到 weather")
	}
	if m.ID != "weather.current" {
		t.Fatalf("命令 id 不对: %s", m.ID)
	}
	if m.Args["city"] != "上海" {
		t.Fatalf("参数解析错了: %v", m.Args)
	}
	if !m.Complete() {
		t.Fatal("必填参数已给，不该报缺")
	}
}

func TestParseCommandMissingRequired(t *testing.T) {
	man := manifestWithCmd(t, "")

	m := ParseCommand(man, "weather")
	if m == nil {
		t.Fatal("光输入命令名也该匹配（只是缺参数）")
	}
	if m.Complete() {
		t.Fatal("没给城市，应该是缺参数")
	}
	if len(m.Missing) != 1 || m.Missing[0] != "city" {
		t.Fatalf("缺失列表不对: %v", m.Missing)
	}
}

func TestParseCommandRestType(t *testing.T) {
	// rest 类型吃掉剩余全部（带空格也算一个参数）
	man := manifestWithCmd(t, "")

	m := ParseCommand(man, "echo 你好 世界 再见")
	if m == nil {
		t.Fatal("应该匹配到 echo")
	}
	if m.Args["content"] != "你好 世界 再见" {
		t.Fatalf("rest 参数应该吃掉全部，实际 %q", m.Args["content"])
	}
}

func TestParseCommandNoArgs(t *testing.T) {
	// 没声明 args 的命令：只填 ID/Name，不解析参数
	man := manifestWithCmd(t, "")

	m := ParseCommand(man, "ping")
	if m == nil {
		t.Fatal("应该匹配到 ping")
	}
	if len(m.Args) != 0 {
		t.Fatalf("没声明参数就不该有 args，实际 %v", m.Args)
	}
}

func TestParseCommandNoMatch(t *testing.T) {
	man := manifestWithCmd(t, "")

	if m := ParseCommand(man, "随便说点什么"); m != nil {
		t.Fatalf("不该匹配: %+v", m)
	}
	if m := ParseCommand(man, ""); m != nil {
		t.Fatal("空文本不该匹配")
	}
}

func TestParseCommandCaseInsensitive(t *testing.T) {
	man := manifestWithCmd(t, "")

	m := ParseCommand(man, "WEATHER 北京")
	if m == nil {
		t.Fatal("命令词应该大小写不敏感")
	}
	if m.Args["city"] != "北京" {
		t.Fatalf("参数不对: %v", m.Args)
	}
}

func TestExtractPlaceholders(t *testing.T) {
	cases := map[string][]string{
		"weather <city>":        {"city"},
		"a <x> <y>":           {"x", "y"},
		"no placeholder":      nil,
		"<only>":              {"only"},
		"tricky <a> text <b>": {"a", "b"},
	}
	for in, want := range cases {
		got := extractPlaceholders(in)
		if len(got) != len(want) {
			t.Errorf("%q: 期望 %v，实际 %v", in, want, got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: 第 %d 个不对，期望 %q 实际 %q", in, i, want[i], got[i])
			}
		}
	}
}

func TestParseCommandAlias(t *testing.T) {
	// 别名也要生成结构化参数，不然插件收到消息却拿不到 args
	man := manifestWithCmd(t, "")

	for _, word := range []string{"p", "乒乓"} {
		m := ParseCommand(man, word)
		if m == nil {
			t.Fatalf("别名 %q 应该匹配到 ping", word)
		}
		if m.ID != "test.ping" {
			t.Fatalf("%q 匹配到了错误的命令: %s", word, m.ID)
		}
		if m.Name != word {
			t.Fatalf("Name 应该是实际匹配到的词 %q，实际 %q", word, m.Name)
		}
	}
}

func TestParseCommandAliasWithArgs(t *testing.T) {
	// 带参数的别名：参数照样解析
	man := manifestWithCmd(t, "")
	m := ParseCommand(man, "P")
	if m == nil {
		t.Fatal("大小写不敏感的别名应该匹配")
	}
	if m.ID != "test.ping" {
		t.Fatalf("匹配错了: %s", m.ID)
	}
}
