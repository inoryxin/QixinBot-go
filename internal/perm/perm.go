package perm

import "github.com/xin/onebot/internal/session"

// Policy 是挂在单个 Feature 上的访问策略。空策略 = 由全局模式决定。
type Policy struct {
	AllowUsers  []int64 `json:"allow_users"`
	AllowGroups []int64 `json:"allow_groups"`
	RequireRole string  `json:"require_role"` // owner | admin
}

// Checker 负责判定"这个主体能否触发这个功能"。
type Checker struct {
	// Mode: allow_all（装完即用）| deny_all（默认拒绝，需显式授权）
	Mode   string
	owners map[int64]bool
}

// New 创建判定器。
func New(mode string, owners []int64) *Checker {
	m := make(map[int64]bool, len(owners))
	for _, o := range owners {
		m[o] = true
	}
	return &Checker{Mode: mode, owners: m}
}

// IsOwner 判断是否框架所有者（不受任何策略限制）。
func (c *Checker) IsOwner(userID int64) bool { return c.owners[userID] }

// Check 返回该会话是否可以触发这个策略。
func (c *Checker) Check(p Policy, s *session.Session) bool {
	if s == nil {
		return false
	}
	// 所有者永远放行，保证不会把自己锁在门外。
	if c.IsOwner(s.UserID) {
		return true
	}
	// deny_all 的语义是"默认拒绝，需显式授权"：
	// 策略里一个授权字段都没有才拒绝；写了 allow_users/allow_groups/
	// require_role 的，走正常评估。之前这里直接 return false，
	// 把所有显式授权都堵死了，deny_all 实际等于"仅主人可用"。
	if c.Mode == "deny_all" && !p.hasGrant() {
		return false
	}

	if len(p.AllowUsers) > 0 && !contains(p.AllowUsers, s.UserID) {
		return false
	}
	if len(p.AllowGroups) > 0 {
		if !s.IsGroup() || !contains(p.AllowGroups, s.ChatID) {
			return false
		}
	}
	if p.RequireRole != "" {
		if !s.IsGroup() {
			return false
		}
		// **owner 是最高级，满足任何 admin 要求**。
		// 之前是严格相等，导致主人被自己设的 require_role="admin" 拦住。
		if s.Role == p.RequireRole || s.Role == "owner" {
			// 通过
		} else {
			return false
		}
	}
	return true
}

// CanSee 用于 /help：能不能看到这条命令（比 Check 宽松，不校验角色）。
func (c *Checker) CanSee(p Policy, s *session.Session) bool {
	if s == nil {
		return false
	}
	if c.IsOwner(s.UserID) {
		return true
	}
	if c.Mode == "deny_all" && !p.hasGrant() {
		return false
	}
	if len(p.AllowUsers) > 0 && !contains(p.AllowUsers, s.UserID) {
		return false
	}
	if len(p.AllowGroups) > 0 && s.IsGroup() && !contains(p.AllowGroups, s.ChatID) {
		return false
	}
	return true
}

// hasGrant 判断策略是否带了显式授权字段。
func (p Policy) hasGrant() bool {
	return len(p.AllowUsers) > 0 || len(p.AllowGroups) > 0 || p.RequireRole != ""
}

func contains(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
