package main

// 回归测试：allow_groups = [] 必须 = 不限。
// 热重载引入 allowList 时，allowed() 对空列表返回 false，
// 导致插件用 call_onebot 的 send_group_msg 全被拒（host.reply 有
// empty() 保护没踩到）——两个出口必须永远同一个语义。

import (
	"testing"
)

func TestAllowListEmptyMeansUnrestricted(t *testing.T) {
	var a allowList
	// 未设置（启动即用，没经过 set）= 不限
	if !a.allowed(123) {
		t.Fatal("未设置的白名单不应拦截")
	}
	// 配置写了 allow_groups = [] = 不限（回归点）
	a.set([]int64{})
	if !a.allowed(123) {
		t.Fatal("空白名单应视为不限，但拦截了群消息")
	}
	// 有成员时只放行成员
	a.set([]int64{111, 222})
	if !a.allowed(111) || !a.allowed(222) {
		t.Fatal("白名单内的群不应被拦")
	}
	if a.allowed(333) {
		t.Fatal("白名单外的群应被拦")
	}
}
