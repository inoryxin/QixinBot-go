package plugin

// 消息/事件分发与调用入口：acquireHost 竞态协调、会话串行、令牌会话、事件队列。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xin/onebot/internal/session"
	"github.com/xin/onebot/internal/trace"
)

// hostAlive 判断 host 的进程是否还活着（自发崩溃时 closing 不会置位，
// 不能只看 closing）。
func hostAlive(h *Host) bool {
	if h == nil {
		return false
	}
	select {
	case <-h.Done():
		return false
	default:
		return true
	}
}

// acquireCall 统一收尾："拿到进程 → 开超时预算 → 调一个方法"。
// 预算在拿到进程后才开：拉起耗时（握手 + ping 可能 15 秒）不该吃掉调用的预算。
// CallTool 这类"拿到 owner 就调"的入口直接用它；
// 能力调用因为要在 acquire 外面套 max_concurrent 限流，暂时保留自己的展开。
func (m *Manager) acquireCall(ctx context.Context, id, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	cur, release, aerr := m.acquireHost(ctx, id)
	if aerr != nil || cur == nil {
		return nil, fmt.Errorf("插件 %s 不可用: %w", id, aerr)
	}
	defer release()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return cur.Call(callCtx, method, params)
}

// acquireHost 是所有"要调插件"的入口的唯一入口：
// 确保在运行 → 取得调用权 → 再取一次当前 Host。
//
// beginCallCtx 等待停止完成时，旧进程可能已停掉甚至被别人重新拉起，
// 手里旧 Host 指针已失效，所以必须重新取。

// acquireHost 是所有"要调插件"的入口的唯一入口：
// 确保在运行 → 取得调用权 → 再取一次当前 Host。
//
// beginCallCtx 等待停止完成时，旧进程可能已停掉甚至被别人重新拉起，
// 手里旧 Host 指针已失效，所以必须重新取。
func (m *Manager) acquireHost(ctx context.Context, id string) (*Host, func(), error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := m.ensureRunning(ctx, id); err != nil {
			lastErr = err
			continue
		}
		if !m.beginCallCtx(ctx, id) {
			lastErr = fmt.Errorf("插件 %s 正在停止，稍后重试", id)
			continue
		}
		m.mu.RLock()
		cur := m.hosts[id]
		m.mu.RUnlock()
		if cur == nil || cur.closing.Load() || !hostAlive(cur) {
			m.endCall(id)
			lastErr = fmt.Errorf("插件 %s 进程已退出", id)
			continue
		}
		released := false
		release := func() {
			if !released {
				released = true
				m.endCall(id)
			}
		}
		return cur, release, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("插件 %s 不可用", id)
	}
	return nil, nil, lastErr
}

// beginCallCtx 在插件正在停止时会等它停完再重试。
// 消息分发用这个：不能因为"刚好在停止"就把这条消息丢掉。

// beginCallCtx 在插件正在停止时会等它停完再重试。
// 消息分发用这个：不能因为"刚好在停止"就把这条消息丢掉。
func (m *Manager) beginCallCtx(ctx context.Context, id string) bool {
	for {
		m.stopMu.Lock()
		if !m.stopping[id] {
			m.inflight[id]++
			m.stopMu.Unlock()
			return true
		}
		done := m.stopDone[id]
		m.stopMu.Unlock()
		if done == nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Second):
			return false
		}
	}
}

// endCall 结束一次在途调用。

// endCall 结束一次在途调用。
func (m *Manager) endCall(id string) {
	m.stopMu.Lock()
	if m.inflight[id] > 0 {
		m.inflight[id]--
	}
	m.stopMu.Unlock()
}

// beginUpgrade 标记插件进入升级窗口（安装器替换文件期间）。

// ensureRunning 确保插件在跑：没在运行就拉起（崩溃恢复、禁用后重新启用）。
// acquireHost 的第一步；进程活着时是一次廉价查询。
func (m *Manager) ensureRunning(ctx context.Context, id string) (*Host, error) {
	if err := m.waitUpgrade(ctx, id); err != nil {
		return nil, err
	}

	m.mu.RLock()
	h := m.hosts[id]
	man := m.metas[id]
	m.mu.RUnlock()
	if hostAlive(h) && !h.closing.Load() {
		return h, nil
	}
	if h != nil {
		// 进程已经死了（崩溃后自动重启又失败的残留）：把尸体摘掉，
		// 否则这里永远返回"存在但已死"的句柄，插件再也醒不过来
		m.mu.Lock()
		if m.hosts[id] == h {
			delete(m.hosts, id)
		}
		m.mu.Unlock()
	}
	if man == nil {
		return nil, fmt.Errorf("插件 %s 不存在", id)
	}
	// 正好在停止：等它真正停完（有完成信号），不是拍脑袋等 1 秒。
	// 等太久就放弃，避免请求无限挂着。
	m.stopMu.Lock()
	stopping := m.stopping[id]
	done := m.stopDone[id]
	m.stopMu.Unlock()
	if stopping && done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			return nil, fmt.Errorf("插件 %s 正在停止，稍后重试", id)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if m.isDisabled(id) {
		return nil, fmt.Errorf("插件 %s 已禁用", id)
	}
	if m.closingManager.Load() {
		return nil, fmt.Errorf("管理器正在关闭")
	}

	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	// 等锁期间可能刚好开始升级：锁内再确认一次
	if err := m.waitUpgrade(ctx, id); err != nil {
		return nil, err
	}
	// 等锁期间可能被禁用了：锁内再确认一次
	if m.isDisabled(id) {
		return nil, fmt.Errorf("插件 %s 已禁用", id)
	}

	m.mu.RLock()
	h = m.hosts[id]
	m.mu.RUnlock()
	if hostAlive(h) && !h.closing.Load() {
		return h, nil
	}
	if h != nil {
		m.mu.Lock()
		if m.hosts[id] == h {
			delete(m.hosts, id)
		}
		m.mu.Unlock()
	}

	m.Log.Info("拉起未运行的插件", "plugin", id)
	if err := m.startPlugin(ctx, id); err != nil {
		return nil, err
	}
	m.mu.RLock()
	h = m.hosts[id]
	m.mu.RUnlock()
	if h == nil {
		// 绝不给调用方返回 (nil, nil)
		return nil, fmt.Errorf("插件 %s 启动后没有 Host（可能刚被禁用）", id)
	}
	return h, nil
}

// SetEnv 线程安全地设置插件进程环境变量（解释器路径等）。

// maxConsecutiveTimeouts 连续这么多次消息超时就自动禁用（挂死自愈）。
// 和崩溃计数同理：给用户一个明确的"它在坏"信号，而不是让整个 bot 变慢。
const maxConsecutiveTimeouts = 5

// bumpTimeout 记一次消息超时，返回当前连击数。任何成功响应都会清零。

// bumpTimeout 记一次消息超时，返回当前连击数。任何成功响应都会清零。
func (m *Manager) bumpTimeout(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timeoutStreak[id]++
	return m.timeoutStreak[id]
}

func (m *Manager) clearTimeout(id string) {
	m.mu.Lock()
	delete(m.timeoutStreak, id)
	m.mu.Unlock()
}

// capPriority 返回作为能力提供者的优先级，未显式设置时沿用 Priority。

// bindCallSession 为一次分发生成令牌并登记可信会话。
func (m *Manager) bindCallSession(s *session.Session) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	m.callMu.Lock()
	m.callSess[token] = s
	m.callMu.Unlock()
	return token
}

func (m *Manager) unbindCallSession(token string) {
	m.callMu.Lock()
	delete(m.callSess, token)
	m.callMu.Unlock()
}

// SessionForToken 用令牌换回可信会话；伪造的令牌换不到任何东西。

// SessionForToken 用令牌换回可信会话；伪造的令牌换不到任何东西。
func (m *Manager) SessionForToken(token string) *session.Session {
	if token == "" {
		return nil
	}
	m.callMu.Lock()
	defer m.callMu.Unlock()
	return m.callSess[token]
}

// Dispatch 按加载顺序把消息交给插件；第一个返回 handled=true 的插件获胜。
// TryAnswer 在消息**进入会话队列之前**检查有没有人在等这个人的回答。
//
// 这一步必须在入口做，不能只在 Dispatch 里做：
// 如果等回答的那条消息还占着会话队列，回答就会被排在它后面，
// 两边互相等死。

// Dispatch 按加载顺序把消息交给插件；第一个返回 handled=true 的插件获胜。
// TryAnswer 在消息**进入会话队列之前**检查有没有人在等这个人的回答。
//
// 这一步必须在入口做，不能只在 Dispatch 里做：
// 如果等回答的那条消息还占着会话队列，回答就会被排在它后面，
// 两边互相等死。
func (m *Manager) TryAnswer(s *session.Session) bool {
	if m.asks == nil || s == nil || s.Text == "" {
		return false
	}
	// 取消词优先
	if IsCancelWord(s.Text) && m.asks.CancelOne(s.Account, s.ChatType, s.ChatID, s.UserID) {
		if s.Trace != nil {
			s.Trace.Add("交互", "", true, "用户主动取消")
		}
		return true
	}
	if m.asks.Deliver(s.Account, s.ChatType, s.ChatID, s.UserID, s.Text) {
		if s.Trace != nil {
			s.Trace.Add("交互", "", true, "被 host.ask 消费")
		}
		return true
	}
	return false
}

func (m *Manager) Dispatch(ctx context.Context, s *session.Session) (*Host, bool) {
	// 交互中的"取消"：优先于一切
	if m.asks != nil && IsCancelWord(s.Text) {
		if m.asks.CancelOne(s.Account, s.ChatType, s.ChatID, s.UserID) {
			if s.Trace != nil {
				s.Trace.Add("交互", "", true, "用户主动取消")
			}
			return nil, true
		}
	}

	// 有人在等这个人的下一条消息？直接投给他，不做插件分发。
	if m.asks != nil && s.Text != "" {
		if m.asks.Deliver(s.Account, s.ChatType, s.ChatID, s.UserID, s.Text) {
			if s.Trace != nil {
				s.Trace.Add("交互", "", true, "被 host.ask 消费")
			}
			return nil, true
		}
	}

	if s.Trace == nil {
		s.Trace = trace.New(s.Text)
	}
	s.Trace.Add("分发", "", true, "开始")

	order := m.dispatchOrder()

	for _, id := range order {
		m.mu.RLock()
		h := m.hosts[id]
		m.mu.RUnlock()
		if h == nil || h.closing.Load() {
			continue
		}
		// 服务插件不接消息，只提供能力
		if h.Manifest.Kind == "service" {
			continue
		}
		// 这条消息是不是在调这个插件声明的命令？
		// 用它区分两种情况，语义完全不同：
		//   是 → 用户明确要用这个功能，被权限/冷却拦下就该**到此为止**
		//   否 → 这条消息跟它无关，静默跳过，别影响后面的插件
		cmdMatched := matchesDeclaredCommand(h.Manifest, s.Text)

		// **带前缀 = 明确在敲命令**：只让"声明了这条命令"的插件处理。
		//
		// 否则 /asdf 会被 ai-chat 当成聊天接掉（它不声明命令、什么都能聊），
		// 用户会以为"命令生效了"，实际是 AI 在编。
		if s.HasPrefix && !cmdMatched {
			continue
		}

		if ok, reason := m.allowWithReason(h.Manifest, s); !ok {
			if m.DebugGate.Load() {
				m.Log.Debug("门禁拦截", "plugin", id, "reason", reason, "text", s.Text)
			}
			if cmdMatched {
				// **停止分发**：否则 /bili 被权限拒之后还会漏给 ai-chat，
				// 用户既收到"没有权限"又被 AI 搭腔，两条回复互相打架。
				s.Trace.Add("权限", id, false, reason)
				m.Log.Info("命令被权限拦下", "plugin", id, "user", s.UserID,
					"text", truncateForLog(s.Text, 40))
				return nil, false
			}
			continue
		}

		// 命令冷却：冷却期内静默跳过（不回"慢点"，那更吵）
		if ok, remain := m.allowCooldown(h, s); !ok {
			if cmdMatched {
				// 同理：明确的命令被冷却，别再让别的插件（尤其 AI）接话
				s.Trace.Add("冷却", id, false, fmt.Sprintf("还需 %d 秒", int(remain.Seconds())+1))
				return nil, false
			}
			continue
		}

		// 结构化命令：插件在 manifest 里声明了 args 就由宿主解析好
		// （没声明的不动，插件照旧自己处理 text）
		packet := s
		if m2 := ParseCommand(h.Manifest, s.Text); m2 != nil {
			cp := *s
			cp.Command = m2
			packet = &cp
		}

		msgTimeout := 30 * time.Second
		if d := h.Manifest.Timeouts.MessageTimeout(); d > 0 {
			msgTimeout = d
		}
		// 同一插件的业务调用串行：插件作者通常不处理并发，多账号同时来消息时
		// 不加锁会把共享状态搅乱。
		cur, release, aerr := m.acquireHost(ctx, id)
		if aerr != nil || cur == nil {
			m.Log.Warn("插件调用放弃", "plugin", id, "err", aerr)
			continue
		}
		h = cur
		// 超时预算从拿到进程后才开始算：拉起（握手 + ping 可能 15 秒）
		// 不该吃掉消息处理的预算，否则刚拉起的插件一来消息就超时
		callCtx, cancel := context.WithTimeout(ctx, msgTimeout)
		unlock := m.lockFor(h, s)
		// 为这次分发生成一次性令牌，插件拿它换可信会话
		token := m.bindCallSession(s)
		packet.CallToken = token
		started := time.Now()
		res, err := h.Call(callCtx, "plugin.handle_message", map[string]any{"session": packet})
		m.unbindCallSession(token)
		s.CallToken = "" // 一次性令牌用完即清：observe 序列化不再携带
		unlock()
		release()
		cancel()

		// 统计：不管成功失败都记一笔
		var handled bool
		if err == nil && len(res) > 0 {
			var probe struct {
				Handled bool `json:"handled"`
			}
			_ = json.Unmarshal(res, &probe)
			handled = probe.Handled
		}
		took := time.Since(started)
		if m.stats != nil {
			m.stats.recordCall(id, took, handled, err)
		}
		if m.OnPluginCall != nil {
			m.OnPluginCall(took.Milliseconds(), err != nil)
		}
		// 连续超时自愈：崩溃有"6 次禁用"保护，"不崩也不回"的挂死之前没有——
		// 它会卡住整个分发循环（后面所有消息排队等超时），bot 表现为整体装死
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			if n := m.bumpTimeout(id); n >= maxConsecutiveTimeouts {
				m.clearTimeout(id)
				m.Log.Error("插件连续消息超时，已自动禁用", "plugin", id, "times", n)
				m.setState(id, "failed", fmt.Sprintf("连续 %d 次消息超时（进程可能卡死），已自动禁用", n))
				go func() { _ = m.Disable(id) }()
				return nil, false
			}
		} else if err == nil {
			m.clearTimeout(id)
		}
		if err != nil {
			s.Trace.AddStage("插件", id, false, err.Error(), took)
		} else if handled {
			s.Trace.AddStage("插件", id, true, "认领", took)
		} else {
			s.Trace.AddStage("插件", id, true, "未认领", took)
		}

		if err != nil {
			m.Log.Error("插件处理失败", "plugin", id, "err", err)
			m.setState(id, "failed", err.Error())
			continue
		}

		var out struct {
			Handled bool `json:"handled"`
		}
		if len(res) > 0 {
			_ = json.Unmarshal(res, &out)
		}
		if out.Handled {
			m.observe(s)
			return h, true
		}
	}
	m.observe(s)
	return nil, false
}

// observe 把消息抄送给声明了 observe = true 的插件（记忆、统计）。
//
// 为什么需要：AI 聊天是 last=true 的兜底插件，前面被 keyword-reply 之类
// 认领的消息它根本收不到——于是"群里说过的话"进不了记忆。
// 观察者是旁路：异步、失败不管、不影响回复。

// observe 把消息抄送给声明了 observe = true 的插件（记忆、统计）。
//
// 为什么需要：AI 聊天是 last=true 的兜底插件，前面被 keyword-reply 之类
// 认领的消息它根本收不到——于是"群里说过的话"进不了记忆。
// 观察者是旁路：异步、失败不管、不影响回复。
func (m *Manager) observe(s *session.Session) {
	m.mu.RLock()
	var list []*Host
	for _, h := range m.hosts {
		if h.Manifest.Observe && !h.closing.Load() {
			list = append(list, h)
		}
	}
	m.mu.RUnlock()
	if len(list) == 0 {
		return
	}

	payload, err := json.Marshal(map[string]any{"session": s})
	if err != nil {
		return
	}
	for _, h := range list {
		go func(h *Host) {
			// 观察者可能做慢事（比如私聊识图），给宽一点；它在独立 goroutine 里不阻塞回复
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := h.Call(ctx, "plugin.observe_message", json.RawMessage(payload)); err != nil {
				m.Log.Debug("观察者处理失败", "plugin", h.Manifest.ID, "err", err)
			}
		}(h)
	}
}

// CheckNames 检查插件中文名是否重复。
// 不阻止加载（可能是有意的），但日志里会警告，WebUI 上会加 id 区分。

// lockFor 决定这次消息处理用哪种锁。
// 默认整插件串行；声明 concurrency = "session" 的插件按会话分片。
func (m *Manager) lockFor(h *Host, s *session.Session) func() {
	if h.Manifest.Concurrency == "session" && s != nil {
		key := s.Account + "|" + s.ChatType + "|" + strconv.FormatInt(s.ChatID, 10)
		return h.LockSession(key)
	}
	h.callMu.Lock()
	return h.callMu.Unlock
}

// dispatchOrder 返回分发顺序：priority 大的先，last 的排最后，同分按 id。

// dispatchOrder 返回分发顺序：priority 大的先，last 的排最后，同分按 id。
func (m *Manager) dispatchOrder() []string {
	m.mu.RLock()
	ids := append([]string(nil), m.order...)
	m.mu.RUnlock()

	sort.SliceStable(ids, func(i, j int) bool {
		li := m.manifestLast(ids[i])
		lj := m.manifestLast(ids[j])
		if li != lj {
			return !li // last 的排后面
		}
		pi, pj := m.orderPriority(ids[i]), m.orderPriority(ids[j])
		if pi != pj {
			return pi > pj
		}
		return ids[i] < ids[j]
	})
	return ids
}

// orderPriority 分发优先级：管理员覆盖优先，其次清单声明。
// 之前只给 i 边取清单值、j 边只在"没有任何覆盖"时才取，
// 比较器两边不对称，less(A,B) 和 less(B,A) 可能同时为真，
// sort 结果不稳定，manifest 里的 priority 实际等于失效。

// orderPriority 分发优先级：管理员覆盖优先，其次清单声明。
// 之前只给 i 边取清单值、j 边只在"没有任何覆盖"时才取，
// 比较器两边不对称，less(A,B) 和 less(B,A) 可能同时为真，
// sort 结果不稳定，manifest 里的 priority 实际等于失效。
func (m *Manager) orderPriority(id string) int {
	if m.PriorityOf != nil {
		if v, ok := m.PriorityOf(id); ok {
			return v
		}
	}
	m.mu.RLock()
	man := m.metas[id]
	m.mu.RUnlock()
	if man != nil {
		return man.Priority
	}
	return 0
}

// manifestLast 读清单的 last 标记（没运行的插件没有 Host，也从 metas 读）。

// manifestLast 读清单的 last 标记（没运行的插件没有 Host，也从 metas 读）。
func (m *Manager) manifestLast(id string) bool {
	m.mu.RLock()
	man := m.metas[id]
	m.mu.RUnlock()
	return man != nil && man.Last
}

// CallCapability 是插件互调入口：只接受 provides 里声明过的能力名。

// DispatchEvent 把非消息事件广播给所有插件。单个插件失败不影响其它插件。
// subscribesEvent 判断插件是否订阅了这条事件。
func subscribesEvent(man *Manifest, ev *session.EventPayload) bool {
	if man == nil || len(man.Events) == 0 {
		return false
	}
	for _, pat := range man.Events {
		switch {
		case pat == "*":
			return true
		case pat == ev.PostType:
			return true
		case strings.HasPrefix(pat, ev.PostType+":"):
			if pat == ev.PostType+":"+ev.NoticeType ||
				pat == ev.PostType+":"+ev.RequestType ||
				pat == ev.PostType+":"+ev.MetaEventType ||
				pat == ev.PostType+":"+ev.SubType {
				return true
			}
		}
	}
	return false
}

func (m *Manager) DispatchEvent(ctx context.Context, ev *session.EventPayload) {
	// 只入队：OneBot 读循环还要收 API 响应，不能被慢插件拖住
	select {
	case m.eventQ <- ev:
	default:
		n := m.eventDrop.Add(1)
		if n == 1 || n%100 == 0 {
			m.Log.Warn("事件队列已满，丢弃事件", "dropped", n, "post_type", ev.PostType)
		}
	}
}

func (m *Manager) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.shutdownCh:
			return
		case ev := <-m.eventQ:
			m.dispatchEventSync(ctx, ev)
		}
	}
}

func (m *Manager) dispatchEventSync(ctx context.Context, ev *session.EventPayload) {
	if m.closingManager.Load() {
		return
	}
	m.mu.RLock()
	ids := append([]string(nil), m.order...)
	m.mu.RUnlock()
	for _, id := range ids {
		if m.isDisabled(id) {
			continue
		}
		// 没订阅这条事件就不发给它——否则一条通知会惊动所有插件
		m.mu.RLock()
		man := m.metas[id]
		m.mu.RUnlock()
		if !subscribesEvent(man, ev) {
			continue
		}
		h, err := m.ensureRunning(ctx, id)
		if err != nil || h == nil {
			continue
		}
		cur, release, aerr := m.acquireHost(ctx, id)
		if aerr != nil || cur == nil {
			continue
		}
		h = cur
		unlock := m.lockFor(h, nil)
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, callErr := h.Call(callCtx, "plugin.handle_event", map[string]any{"event": ev})
		unlock()
		release()
		cancel()
		if callErr != nil {
			m.Log.Warn("事件处理失败", "plugin", id, "type", ev.PostType, "err", callErr)
		}
	}
}

// ListTools 汇总所有插件的工具声明。

// matchesDeclaredCommand 判断消息是不是在调某个插件声明的命令。
//
// 只看 manifest 的 [[commands]]（含 aliases），不看插件自己的逻辑——
// 宿主不可能知道插件内部怎么匹配，所以用"声明过没有"当判据。
func matchesDeclaredCommand(man *Manifest, text string) bool {
	for i := range man.Commands {
		if _, ok := matchCommandLen(man.Commands[i], text); ok {
			return true
		}
	}
	return false
}

// truncateForLog 截断日志里的用户输入（避免刷屏，也避免日志里出现超长消息）。
