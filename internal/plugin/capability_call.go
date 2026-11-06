package plugin

// 能力调用侧：Manager 的能力注册表、按优先级选提供者、失败重试。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xin/onebot/internal/session"
)

// capProvider 是一个能力的提供者。同名能力可以有多个，priority 高的赢。
type capProvider struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
}

// Status 是给 WebUI / 日志看的插件状态。

// capPriority 返回作为能力提供者的优先级，未显式设置时沿用 Priority。
func capPriority(man *Manifest) int {
	if man.CapabilityPriority != nil {
		return *man.CapabilityPriority
	}
	return man.Priority
}

// capName 从 "tts.speak@1" 里取出 "tts.speak"。

// capName 从 "tts.speak@1" 里取出 "tts.speak"。
func capName(decl string) string {
	decl = strings.TrimSpace(decl)
	if i := strings.Index(decl, "@"); i > 0 {
		decl = decl[:i]
	}
	return decl
}

// supervise 看住一个插件进程：意外退出就退避重启，连续失败则禁用。

// CallCapability 是插件互调入口：只接受 provides 里声明过的能力名。
func (m *Manager) CallCapability(ctx context.Context, name string, args map[string]any, s *session.Session) (json.RawMessage, error) {
	// 框架内置的能力优先（例如 db.sqlite —— 框架自己就有 SQLite，
	// 没必要再让插件绕一圈 RPC）
	if m.Builtins != nil {
		if fn, ok := m.Builtins[name]; ok {
			data, err := fn(ctx, args)
			if err != nil {
				return nil, err
			}
			// 插件侧期望的是 {ok, data}，这里保持一致
			wrapped, _ := json.Marshal(map[string]any{"ok": true, "data": data})
			return json.RawMessage(wrapped), nil
		}
	}

	m.mu.RLock()
	list := append([]capProvider(nil), m.caps[name]...)
	m.mu.RUnlock()
	if len(list) == 0 {
		return nil, fmt.Errorf("能力 %s 没有提供者（检查插件的 provides）", name)
	}
	params := map[string]any{"name": name, "args": args}
	if s != nil {
		params["session"] = s
	}

	// 按优先级依次尝试：
	//   - 提供者没运行 → 试下一个
	//   - 声明了 retry="always" 且错误是超时/不可用/内部错误 → 自动降级
	//   - 其余失败（含参数不合法 / 权限不足）→ 立刻返回：
	//     能力可能有副作用，超时后原提供者未必停下，换一个再执行可能重复
	var lastErr error
	tried := 0
	for _, prov := range list {
		m.mu.RLock()
		h := m.hosts[prov.ID]
		m.mu.RUnlock()
		if h == nil || h.closing.Load() {
			// 能力调用是明确请求：没在运行的提供者直接拉起
			nh, err := m.ensureRunning(ctx, prov.ID)
			if err != nil {
				lastErr = fmt.Errorf("能力 %s 的提供者 %s 未运行: %w", name, prov.ID, err)
				continue
			}
			h = nh
		}
		if s != nil && !m.allow(h.Manifest, s) {
			// 权限是全局策略，换提供者不该绕过它
			return nil, fmt.Errorf("当前用户无权调用 %s", name)
		}
		// 提供者声明了参数 schema 就校验，防止两边约定对不上
		if spec := findCapSpec(h.Manifest, name); spec != nil {
			if err := validateArgs(spec.Parameters, args); err != nil {
				return nil, fmt.Errorf("能力 %s 参数不合法: %w", name, err)
			}
		}

		if m.DebugGate.Load() || m.Log.Enabled(context.Background(), slog.LevelDebug) {
			m.Log.Debug("能力调用", "capability", name, "provider", prov.ID,
				"candidates", len(list), "attempt", tried+1)
		}

		capTimeout := 15 * time.Second
		if d := h.Manifest.Timeouts.CapabilityTimeout(); d > 0 {
			capTimeout = d
		}
		callCtx, cancel := context.WithTimeout(ctx, capTimeout)

		// 提供者声明了 max_concurrent 就在这里排队，
		// 别让调用方干等到超时才报"超时"——它其实是"忙"
		limit := 0
		if spec := findCapSpec(h.Manifest, name); spec != nil {
			limit = spec.MaxConcurrent
		}
		release, lerr := m.capLim.Acquire(callCtx, prov.ID, name, limit)
		if lerr != nil {
			cancel()
			return nil, Errf(CodeRateLimited, "%v", lerr)
		}
		// 只有真正要调用时才计在途：前面的 schema 校验、排队失败都不算，
		// 否则计数泄漏，看起来像一直有调用在途。
		// 统一入口：确保运行 + 在途计数 + 再取当前 Host
		cur, releaseCall, aerr := m.acquireHost(ctx, prov.ID)
		if aerr != nil || cur == nil {
			release()
			cancel()
			lastErr = fmt.Errorf("提供者 %s 取不到可用进程: %w", prov.ID, aerr)
			continue
		}
		h = cur
		res, err := h.Call(callCtx, "plugin.call_tool", params)
		releaseCall()
		release()
		cancel()
		tried++

		if err == nil {
			if tried > 1 {
				m.Log.Info("能力调用降级成功", "capability", name,
					"used", prov.ID, "attempts", tried)
			}
			return res, nil
		}
		lastErr = err

		// **默认不重试**：能力可能有副作用（发消息、扣款、控设备），
		// 超时后原提供者未必已经停下，换一个再执行一次就可能重复。
		//
		// 只有 manifest 明确写了 retry = "always" 才在失败后切换，
		// 且只对"换一个提供者可能成功"的错误切（超时/不可用/内部错误/进程挂了）；
		// 参数、权限这类错误换谁都会错，立刻返回。
		if spec := findCapSpec(h.Manifest, name); spec != nil && spec.Retry == "always" {
			if retryableCapErr(err) {
				m.Log.Warn("能力提供者失败，尝试下一个（retry=always）", "capability", name,
					"provider", prov.ID, "err", err)
				continue
			}
			return nil, err
		}
		return nil, err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("能力 %s 没有可用的提供者", name)
	}
	return nil, lastErr
}

// retryableCapErr 判断"换一个提供者有没有可能成功"。
//
// 只有"这个提供者暂时不行"才值得重试：
//   - 超时 / 内部错误 / 暂时不可用
//
// 参数错、权限错换谁都一样，直接返回。

// retryableCapErr 判断"换一个提供者有没有可能成功"。
//
// 只有"这个提供者暂时不行"才值得重试：
//   - 超时 / 内部错误 / 暂时不可用
//
// 参数错、权限错换谁都一样，直接返回。
func retryableCapErr(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		// 不是 RPC 错误（比如进程挂了）也值得试下一个
		return true
	}
	switch rpcErr.Code {
	case CodeTimeout, CodeUnavailable, CodeInternalError:
		return true
	default:
		return false
	}
}

// allow 判定"这个用户能不能用这个插件"。
// 优先级：插件级 [access] → 命中命令的命令级覆盖 → WebUI 覆盖（按插件）。

// ListCapabilities 返回能力名 → 提供者列表（已按优先级排序），供 WebUI 展示"谁接管了这个能力"。
func (m *Manager) ListCapabilities() map[string][]capProvider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]capProvider, len(m.caps))
	for k, v := range m.caps {
		out[k] = append([]capProvider(nil), v...)
	}
	return out
}

// ResolveCapability 返回能力当前的实际提供者（优先级最高的那个）。

// ResolveCapability 返回能力当前的实际提供者（优先级最高的那个）。
func (m *Manager) ResolveCapability(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.caps[name]
	if len(list) == 0 {
		return "", false
	}
	return list[0].ID, true
}

// HostOf 返回某个插件的运行句柄；未运行时返回 nil。

// CapabilitiesOf 返回某个插件声明的能力名列表。
func (m *Manager) CapabilitiesOf(id string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	man, ok := m.metas[id]
	if !ok || man == nil {
		return nil
	}
	out := make([]string, 0, len(man.Provides))
	for _, p := range man.Provides {
		out = append(out, capName(p))
	}
	return out
}

// matchesDeclaredCommand 判断消息是不是在调某个插件声明的命令。
//
// 只看 manifest 的 [[commands]]（含 aliases），不看插件自己的逻辑——
// 宿主不可能知道插件内部怎么匹配，所以用"声明过没有"当判据。
// HookResult 是一次分发钩子的返回。
type HookResult struct {
	Provider string
	Data     json.RawMessage
	Err      error
}

// CallCapabilityEach 依次调用某个能力的**所有**提供者（按优先级）。
//
// 和 CallCapability 的区别：这里不做"抢一个提供者"的语义，而是广播——
// 每个提供者都有机会表态，用于 core.dispatch 这类统一钩子。
// 单个提供者失败不影响其它提供者；不做用户权限门禁（钩子是框架机制，不是用户请求）。
func (m *Manager) CallCapabilityEach(ctx context.Context, name string, args map[string]any, s *session.Session) []HookResult {
	m.mu.RLock()
	list := append([]capProvider(nil), m.caps[name]...)
	m.mu.RUnlock()
	out := make([]HookResult, 0, len(list))
	if len(list) == 0 {
		return out
	}
	params := map[string]any{"name": name, "args": args}
	if s != nil {
		params["session"] = s
	}
	for _, prov := range list {
		cur, releaseCall, aerr := m.acquireHost(ctx, prov.ID)
		if aerr != nil || cur == nil {
			out = append(out, HookResult{Provider: prov.ID, Err: aerr})
			continue
		}
		timeout := 5 * time.Second
		if d := cur.Manifest.Timeouts.CapabilityTimeout(); d > 0 {
			timeout = d
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		res, err := cur.Call(callCtx, "plugin.call_tool", params)
		releaseCall()
		cancel()
		out = append(out, HookResult{Provider: prov.ID, Data: res, Err: err})
	}
	return out
}