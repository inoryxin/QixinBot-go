package main

// 配置热重载：每 5 秒看一眼 config.toml 的 mtime，变了就重读，
// 把"热字段"套到运行中的对象上。改配置不再需要重启 bot。
//
// 热字段白名单（刻意很小，都是无状态、改了立即生效的）：
//   - [log] level / gate_debug
//   - [send] interval_ms / allow_groups
// 账号、协议端地址这类需要重连的字段**不在**热重载范围——改了它们仍要重启。

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/xin/onebot/internal/config"
	"github.com/xin/onebot/internal/plugin"
	"github.com/xin/onebot/internal/send"
)

// allowList 是 send.allow_groups 的热快照。
// 空 = 不限；host.reply 和 host.call_onebot 两个出口共用这一份，
// 改配置瞬间两边同时生效，不会出现"一边新一边旧"。
type allowList struct {
	v atomic.Pointer[[]int64]
}

func (a *allowList) set(groups []int64) {
	if groups == nil {
		groups = []int64{}
	}
	a.v.Store(&groups)
}

// allowed 判断 gid 是否可发。**空列表 = 不限**（与配置注释一致），
// host.reply 和 host.call_onebot 两个出口走同一个判断，行为永远一致。
func (a *allowList) allowed(gid int64) bool {
	groups := a.v.Load()
	if groups == nil || len(*groups) == 0 {
		return true
	}
	for _, g := range *groups {
		if g == gid {
			return true
		}
	}
	return false
}

type hotReloader struct {
	cfgPath string
	level   *slog.LevelVar
	mgr     *plugin.Manager
	queue   *send.Queue
	allow   *allowList
	log     *slog.Logger
	lastMod time.Time
}

// startHotReload 先对齐一次当前配置，然后起 5 秒轮询。
func startHotReload(ctx context.Context, cfgPath string, level *slog.LevelVar, mgr *plugin.Manager, queue *send.Queue, allow *allowList, log *slog.Logger) *hotReloader {
	h := &hotReloader{cfgPath: cfgPath, level: level, mgr: mgr, queue: queue, allow: allow, log: log}
	h.apply() // 初始对齐（配置文件刚被读过一次，mtime 对齐即可）
	if st, err := os.Stat(cfgPath); err == nil {
		h.lastMod = st.ModTime()
	}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				st, err := os.Stat(h.cfgPath)
				if err != nil || st.ModTime().Equal(h.lastMod) {
					continue
				}
				h.lastMod = st.ModTime()
				h.apply()
			}
		}
	}()
	return h
}

// apply 重读配置并套用热字段；解析失败沿用旧值（文件改一半/手抖都有救）。
func (h *hotReloader) apply() {
	cfg, err := config.Load(h.cfgPath)
	if err != nil {
		h.log.Warn("配置热重载失败，沿用旧配置", "err", err)
		return
	}
	h.level.Set(parseLevel(cfg.Log.Level))
	h.mgr.DebugGate.Store(cfg.Log.GateDebug)
	h.queue.SetInterval(time.Duration(cfg.Send.IntervalMS) * time.Millisecond)
	h.allow.set(cfg.Send.AllowGroups)
	h.log.Info("配置已热重载",
		"log_level", cfg.Log.Level, "gate_debug", cfg.Log.GateDebug,
		"interval_ms", cfg.Send.IntervalMS, "allow_groups", len(cfg.Send.AllowGroups))
}
