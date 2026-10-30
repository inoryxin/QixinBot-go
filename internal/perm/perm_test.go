package perm

import (
	"testing"

	"github.com/xin/onebot/internal/session"
)

func sess(userID, groupID int64, hasPrefix, mentioned bool) *session.Session {
	s := &session.Session{UserID: userID, HasPrefix: hasPrefix, Mentioned: mentioned}
	if groupID > 0 {
		s.ChatType = "group"
		s.ChatID = groupID
	} else {
		s.ChatType = "private"
		s.ChatID = userID
	}
	return s
}

func TestOwnerAlwaysAllowed(t *testing.T) {
	c := New("allow_all", []int64{1})
	// 所有者即使名单里没有、没前缀、没 @，也必须放行
	p := Policy{AllowUsers: []int64{999}}
	if !c.Check(p, sess(1, 0, false, false)) {
		t.Fatal("所有者应该永远放行")
	}
}

func TestDenyAllMode(t *testing.T) {
	c := New("deny_all", nil)
	if c.Check(Policy{}, sess(42, 0, true, true)) {
		t.Fatal("deny_all 模式下非所有者应该被拒")
	}
}

func TestDenyAllHonorsExplicitGrants(t *testing.T) {
	// deny_all 的文档语义是"默认拒绝，需显式授权"：
	// 之前实现对非所有者直接 return false，allow 列表成了摆设
	c := New("deny_all", nil)

	users := Policy{AllowUsers: []int64{100}}
	if !c.Check(users, sess(100, 0, true, true)) {
		t.Fatal("deny_all 下显式 allow_users 应该放行")
	}
	if c.Check(users, sess(300, 0, true, true)) {
		t.Fatal("deny_all 下不在 allow_users 里的应该被拒")
	}

	groups := Policy{AllowGroups: []int64{500}}
	if !c.Check(groups, sess(999, 500, true, true)) {
		t.Fatal("deny_all 下显式 allow_groups 应该放行")
	}
	if c.Check(groups, sess(999, 600, true, true)) {
		t.Fatal("deny_all 下不在 allow_groups 里的应该被拒")
	}

	// CanSee 同理：授权过的人要能在 /help 里看到
	if !c.CanSee(users, sess(100, 0, false, false)) {
		t.Fatal("deny_all 下显式授权的命令应该在 /help 可见")
	}
	if c.CanSee(Policy{}, sess(100, 0, false, false)) {
		t.Fatal("deny_all 下无授权的命令不应该在 /help 可见")
	}
}

func TestAllowUsersWhitelist(t *testing.T) {
	c := New("allow_all", nil)
	p := Policy{AllowUsers: []int64{100, 200}}
	if !c.Check(p, sess(100, 0, false, false)) {
		t.Fatal("白名单内的用户应该放行")
	}
	if c.Check(p, sess(300, 0, false, false)) {
		t.Fatal("白名单外的用户应该被拒")
	}
}

func TestAllowGroups(t *testing.T) {
	c := New("allow_all", nil)
	p := Policy{AllowGroups: []int64{500}}
	if !c.Check(p, sess(1, 500, false, false)) {
		t.Fatal("白名单内的群应该放行")
	}
	if c.Check(p, sess(1, 600, false, false)) {
		t.Fatal("白名单外的群应该被拒")
	}
}

func TestCanSeeFiltersHelp(t *testing.T) {
	c := New("allow_all", nil)
	p := Policy{AllowUsers: []int64{100}}

	if !c.CanSee(p, sess(100, 0, false, false)) {
		t.Fatal("白名单内的用户应该能在 /help 里看到")
	}
	if c.CanSee(p, sess(300, 0, false, false)) {
		t.Fatal("白名单外的用户不应该在 /help 里看到")
	}
}

func TestOwnerSatisfiesAdmin(t *testing.T) {
	// 主人是最高级，应该满足 require_role = "admin"
	// （之前是严格相等，主人被自己的配置拦住）
	c := New("allow_all", []int64{1})

	admin := Policy{RequireRole: "admin"}
	if !c.Check(admin, sess(1, 500, false, false)) {
		t.Fatal("主人应该满足 admin 要求")
	}

	// 普通成员不满足
	if c.Check(admin, sess(999, 500, false, false)) {
		t.Fatal("普通成员不该满足 admin")
	}

	// owner 要求：只有主人能过
	owner := Policy{RequireRole: "owner"}
	if !c.Check(owner, sess(1, 500, false, false)) {
		t.Fatal("主人应该满足 owner")
	}
	if c.Check(owner, sess(999, 500, false, false)) {
		t.Fatal("普通成员不该满足 owner")
	}
}

func TestRequireRoleNeedsGroup(t *testing.T) {
	// 私聊没有群角色，require_role 直接拒
	c := New("allow_all", []int64{1})
	p := Policy{RequireRole: "admin"}

	s := &session.Session{UserID: 999, ChatType: "private", ChatID: 999, Role: "owner"}
	if c.Check(p, s) {
		t.Fatal("私聊不该满足群角色要求")
	}
}
