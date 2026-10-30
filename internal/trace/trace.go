// Package trace 记录一条消息从收到到回复的完整路径。
//
// 为什么需要它：插件没响应的时候，插件作者要在日志里翻半天才知道
// 是"没匹配上命令"、"被权限拦了"、"插件崩了"还是"回复没发出去"。
//
// 有了 trace，一条消息走到哪一步、卡在哪、为什么，一眼能看到。
package trace

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Stage 是一个处理阶段。
type Stage struct {
	Name    string        `json:"name"`              // 阶段名：收到 / 权限 / 分发 / 插件 / 发送
	Plugin  string        `json:"plugin,omitempty"`  // 相关插件
	OK      bool          `json:"ok"`                // 这一步过没过
	Reason  string        `json:"reason,omitempty"`  // 没过或备注的原因
	Took    time.Duration `json:"-"`                 // 耗时
	TookMS  int64         `json:"took_ms"`           // 毫秒
	At      time.Time     `json:"-"`                 // 发生时刻
}

// Trace 是一条消息的追踪记录。
type Trace struct {
	mu     sync.Mutex `json:"-"`
	ID     string     `json:"id"`
	Text   string     `json:"text"`
	Stages []Stage    `json:"stages"`
	start  time.Time
	ended  bool
}

// New 开始追踪一条消息。
func New(text string) *Trace {
	return &Trace{
		ID:     newID(),
		Text:   truncate(text, 80),
		Stages: make([]Stage, 0, 8),
		start:  time.Now(),
	}
}

// Add 记一个阶段。
func (t *Trace) Add(name, plugin string, ok bool, reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.Stages = append(t.Stages, Stage{
		Name:   name,
		Plugin: plugin,
		OK:     ok,
		Reason: reason,
		At:     now,
		Took:   now.Sub(t.start),
		TookMS: now.Sub(t.start).Milliseconds(),
	})
}

// AddStage 记一个带独立耗时的阶段（比如插件调用花了多久）。
func (t *Trace) AddStage(name, plugin string, ok bool, reason string, took time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Stages = append(t.Stages, Stage{
		Name:   name,
		Plugin: plugin,
		OK:     ok,
		Reason: reason,
		Took:   took,
		TookMS: took.Milliseconds(),
		At:     time.Now(),
	})
}

// End 标记结束。
func (t *Trace) End() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ended {
		t.ended = true
	}
}

// TotalMS 是总耗时。
func (t *Trace) TotalMS() int64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Since(t.start).Milliseconds()
}

// Verdict 用一句话总结这条消息怎么了。
func (t *Trace) Verdict() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.Stages) == 0 {
		return "没有记录"
	}
	// 看最后一个失败阶段
	for i := len(t.Stages) - 1; i >= 0; i-- {
		s := t.Stages[i]
		if !s.OK {
			if s.Reason != "" {
				return fmt.Sprintf("%s：%s", s.Name, s.Reason)
			}
			return s.Name + " 未通过"
		}
	}
	return "处理完成"
}

// Snapshot 返回可序列化的副本。
func (t *Trace) Snapshot() map[string]any {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]any{
		"id":       t.ID,
		"text":     t.Text,
		"stages":   t.Stages,
		"total_ms": time.Since(t.start).Milliseconds(),
		"verdict":  t.VerdictLocked(),
	}
}

// VerdictLocked 是不加锁的 Verdict（内部用）。
//
// 优先报"原因"而不是"结果"：
//   - 权限拦了 → 说权限（不是"没有回复"）
//   - 插件报错 → 说报错
//   - 都没有、只是没回复 → 才说没有回复
func (t *Trace) VerdictLocked() string {
	// **先看有没有插件成功认领**。
	// 有的话，中途那些"冷却/权限"都只是路过（别的插件被拦），
	// 报它们会误导——用户明明拿到了回复。
	// **"认领"不等于"回复发出去了"**。
	// 插件认领后是它自己调 host.reply 发的，框架看不到结果。
	// 所以这里说"已认领"而不是"处理完成"——后者会误导人以为消息一定到了。
	for _, s := range t.Stages {
		if s.Name == "插件" && s.OK && s.Reason == "认领" {
			return "已认领（回复由插件发送）"
		}
	}

	// **权限**比冷却更根本：被权限拦说明你根本不能用，
	// 不是"等一会儿就行"。所以先报权限。
	for _, s := range t.Stages {
		if !s.OK && s.Name == "权限" {
			if s.Plugin != "" {
				return "权限（" + s.Plugin + "）：" + s.Reason
			}
			return "权限：" + s.Reason
		}
	}

	// 冷却：**列出所有被冷却的插件**——一次消息会依次问过所有插件，
	// 只报第一个会让人误以为是目标插件（实际可能是路过的）。
	var cooled []string
	for _, s := range t.Stages {
		if !s.OK && s.Name == "冷却" {
			// 每个插件带自己的原因，别让 10 秒错配到 2 秒的插件上
			label := s.Plugin
			if label == "" {
				label = "?"
			}
			if s.Reason != "" {
				cooled = append(cooled, label+" "+s.Reason)
			} else {
				cooled = append(cooled, label)
			}
		}
	}
	if len(cooled) > 0 {
		return "冷却：" + strings.Join(cooled, "、")
	}
	// 第一优先：权限 / 插件失败
	for _, s := range t.Stages {
		if s.OK {
			continue
		}
		if s.Name == "权限" || s.Name == "插件" {
			if s.Reason != "" {
				return fmt.Sprintf("%s（%s）：%s", s.Name, s.Plugin, s.Reason)
			}
			return fmt.Sprintf("%s（%s）未通过", s.Name, s.Plugin)
		}
	}
	// 被交互流程消费：这条消息本身就是"回答"，不用再发东西
	for _, s := range t.Stages {
		if s.Name == "交互" && s.OK {
			return "已被交互流程消费（插件会接着处理）"
		}
	}
	// 第二优先：发送
	for _, s := range t.Stages {
		if !s.OK && s.Name == "发送" {
			return "发送：" + s.Reason
		}
	}
	return "处理完成"
}

// ---------------------------------------------------------------- 工具

var idMu sync.Mutex
var idSeq uint64

// newID 生成一个短 ID（进程内自增 + 时间后缀），够日志和 UI 用。
func newID() string {
	idMu.Lock()
	idSeq++
	n := idSeq
	idMu.Unlock()
	return fmt.Sprintf("t%d-%x", n, time.Now().UnixNano()&0xffff)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// FailedStages 返回所有没通过的阶段（给外面判断原因用）。
func (t *Trace) FailedStages() []Stage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Stage
	for _, s := range t.Stages {
		if !s.OK {
			out = append(out, s)
		}
	}
	return out
}
