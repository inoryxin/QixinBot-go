package main

// 会话 worker 池：同会话串行、空闲回收。

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xin/onebot/internal/session"
)

// sessionPool 是"按会话分队列 + 每会话一个 worker"的池子。
//
// 为什么要按会话分：
//   - 同一个群必须保序（连发两条指令不能乱）
//   - 不同群必须并行（一个群慢不该拖累别的）
type sessionPool struct {
	ctx     context.Context
	handle  func(context.Context, *session.Session)
	log     *slog.Logger
	mu      sync.Mutex
	workers map[string]*sessionWorker
	wg      sync.WaitGroup
	dropped int64 // 因队列满被丢弃的消息数
}

type sessionWorker struct {
	ch       chan *session.Session
	lastUsed atomic.Int64 // unix 秒，用于空闲回收
}

// queueDepth 是单个会话的积压上限。超了就丢，避免内存无限涨。

// queueDepth 是单个会话的积压上限。超了就丢，避免内存无限涨。
const queueDepth = 64

func newSessionPool(ctx context.Context, handle func(context.Context, *session.Session), log *slog.Logger) *sessionPool {
	return &sessionPool{
		ctx:     ctx,
		handle:  handle,
		log:     log,
		workers: make(map[string]*sessionWorker),
	}
}

// Submit 投递消息。同一个会话串行，不同会话并行。

// Submit 投递消息。同一个会话串行，不同会话并行。
func (p *sessionPool) Submit(s *session.Session, log *slog.Logger) {
	key := fmt.Sprintf("%s|%s:%d", s.Account, s.ChatType, s.ChatID)

	// **投递必须在锁内做**。
	//
	// 否则会出现：worker 判定"队列空"准备退出，Submit 这时拿到
	// map 里的旧 worker 把消息塞进去——没人读了，消息悄悄卡住。
	// 锁住之后，worker 的"检查空 + 摘除"和这里的"取 worker + 投递"
	// 互斥，要么投给活着的 worker，要么新建一个。
	p.mu.Lock()
	w, ok := p.workers[key]
	if !ok {
		w = &sessionWorker{ch: make(chan *session.Session, queueDepth)}
		p.workers[key] = w
		p.wg.Add(1)
		go p.runWorker(w, key)
	}
	w.lastUsed.Store(time.Now().Unix())
	select {
	case w.ch <- s:
		p.mu.Unlock()
	default:
		// 队列满：丢掉并记一笔。总比无限堆积把内存吃光好。
		p.mu.Unlock()
		atomic.AddInt64(&p.dropped, 1)
		log.Warn("会话队列已满，丢弃消息", "key", key, "user", s.UserID, "text", truncateText(s.Text, 40))
	}
}

// sessionIdleTimeout 是会话 worker 的空闲回收时间。
// 私聊用户多、运行时间长时，不回收会让 goroutine 数一直涨。

// sessionIdleTimeout 是会话 worker 的空闲回收时间。
// 私聊用户多、运行时间长时，不回收会让 goroutine 数一直涨。
const sessionIdleTimeout = 10 * time.Minute

// forgetWorker 把已经退出的 worker 从 map 里移除。

// forgetWorker 把已经退出的 worker 从 map 里移除。
func (p *sessionPool) forgetWorker(key string, w *sessionWorker) {
	p.mu.Lock()
	if cur, ok := p.workers[key]; ok && cur == w {
		delete(p.workers, key)
	}
	p.mu.Unlock()
}

// 说明：正常空闲回收在 idle 分支里摘除（和投递互斥）；
// forgetWorker 只是 Close / 异常退出的兜底。

func (p *sessionPool) runWorker(w *sessionWorker, key string) {
	defer p.wg.Done()
	defer p.forgetWorker(key, w)

	idle := time.NewTimer(sessionIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-idle.C:
			// 空闲太久：**在同一把锁里**确认队列空并摘除自己。
			// 和 Submit 的投递互斥，不会出现"投给已退出的 worker"。
			p.mu.Lock()
			empty := len(w.ch) == 0 && p.workers[key] == w
			if empty {
				delete(p.workers, key)
			}
			p.mu.Unlock()
			if empty {
				return
			}
			idle.Reset(sessionIdleTimeout)
		case s, ok := <-w.ch:
			if !ok {
				return
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(sessionIdleTimeout)
			func() {
				defer func() {
					// 插件把宿主搞崩时别让整个 worker 死掉
					if r := recover(); r != nil {
						p.log.Error("处理消息时 panic", "key", key, "err", r)
					}
				}()
				p.handle(p.ctx, s)
			}()
		}
	}
}

// Stats 给监控用。

// Stats 给监控用。
func (p *sessionPool) Stats() (active int, dropped int64) {
	p.mu.Lock()
	active = len(p.workers)
	p.mu.Unlock()
	dropped = atomic.LoadInt64(&p.dropped)
	return
}

func (p *sessionPool) Close() {
	// 先摘下快照再关：避免和 forgetWorker 抢（它也会动 map）
	p.mu.Lock()
	workers := p.workers
	p.workers = make(map[string]*sessionWorker)
	p.mu.Unlock()

	for _, w := range workers {
		close(w.ch)
	}
	p.wg.Wait()
}
