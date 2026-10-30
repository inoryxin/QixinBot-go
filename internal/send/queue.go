package send

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xin/onebot/internal/onebot"
)

type result struct {
	id  int64
	err error
}

type job struct {
	// ctx 是调用方的上下文。worker 发送前会检查它——
	// 调用方超时/取消后，排队的任务不该再发出去。
	ctx      context.Context
	chatType string
	chatID   int64
	segments []onebot.Segment
	replyTo  int64
	done     chan result
}

type chatQueue struct {
	// lastUsed 是最后一次投递时间（unix 秒），空闲回收用。
	lastUsed atomic.Int64

	ch chan job
}

// Queue 保证同一个 (账号, 会话) 的消息串行发出，并做最小间隔限频。
// 所有插件发送都必须经过这里，不允许直接调 onebot.Call。
type Queue struct {
	clients  map[string]*onebot.Client
	interval atomic.Int64 // 发送间隔（ns），热重载可改
	onSent   func()       // 成功投递回调（指标）
	log      *slog.Logger
	ctx      context.Context

	mu     sync.Mutex
	queues map[string]*chatQueue
	order  []string // 账号顺序（config 顺序），DefaultAccount 的确定性兜底

	// capture 非空时，所有发送都被拦截到这里，不真的发出去。
	// 用 atomic.Value 而不是 Mutex：Send 是热路径，绝不能被测试工具阻塞。
	// （之前用 Mutex 导致注入期间所有真实消息的发送都卡住）
	capture atomic.Value // 存 captureHolder

	// muted 是全局静音开关：true 时所有 Send 直接丢弃。
	// 测试注入时打开，双保险——即使 capture 因为超时/并发失效，也不会真的发到群里。
	// 引用计数：多个注入并发时，先结束的那个不能把别人的静音一起解掉。
	muted     atomic.Bool
	muteMu    sync.Mutex
	muteCount int

	// 捕获代数：stop 时只清掉自己那一代，别把后来者顶掉又抢着收尾
	capMu  sync.Mutex
	capGen uint64
}

// captureHolder 包一层，因为 atomic.Value 不能存 nil。
type captureHolder struct {
	gen uint64
	fn  func(CapturedMessage)
}

// CapturedMessage 是被拦截下来的一条待发消息。
type CapturedMessage struct {
	Account  string           `json:"account"`
	ChatType string           `json:"chat_type"`
	ChatID   int64            `json:"chat_id"`
	Segments []onebot.Segment `json:"segments"`
	ReplyTo  int64            `json:"reply_to"`
}

// Mute 打开全局静音，返回恢复函数。测试注入期间用，防止误发到真实群。
// 引用计数：并发注入各自拿一把锁，计数归零才真正解除；
// 释放函数加锁是为了避免"释放和新的 Mute 交错"把计数扣成假零。
func (q *Queue) Mute() func() {
	q.muteMu.Lock()
	q.muteCount++
	q.muted.Store(true)
	q.muteMu.Unlock()
	return func() {
		q.muteMu.Lock()
		q.muteCount--
		if q.muteCount <= 0 {
			q.muteCount = 0
			q.muted.Store(false)
		}
		q.muteMu.Unlock()
	}
}

// IsCapturing 判断当前是否在捕获模式（测试注入）。
// 调用方据此决定要不要记聊天记录——捕获的消息没真发，记进去会误导。
func (q *Queue) IsCapturing() bool {
	h, _ := q.capture.Load().(captureHolder)
	return h.fn != nil
}

// BeginCapture 开始拦截发送，返回一个停止函数。
// 并发注入时后来者顶掉前者；前者的延迟 stop 不得动后来者的捕获，
// 所以按代数判断，只有自己还是当前一代时才清空。
func (q *Queue) BeginCapture(fn func(CapturedMessage)) func() {
	q.capMu.Lock()
	q.capGen++
	gen := q.capGen
	q.capMu.Unlock()
	q.capture.Store(captureHolder{gen: gen, fn: fn})
	return func() {
		q.capMu.Lock()
		if q.capGen == gen {
			q.capture.Store(captureHolder{gen: gen})
		}
		q.capMu.Unlock()
	}
}

// New 创建发送队列。ctx 用于 worker 的生命周期。
// DefaultAccount 返回唯一已连接账号的 id。
// 单账号框架下插件不用指定 account，宿主自己兜。
// DefaultAccount 返回默认发送账号：多账号时按 config 顺序取第一个
// （Go map 遍历无序，之前每次调用结果都可能不同——ask 的提示语和
// 会话落到不同账号，等待键就对不上了）。
func (q *Queue) DefaultAccount() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) > 0 {
		return q.order[0]
	}
	// order 未注入（测试/极端场景）：按字典序至少确定性
	ids := make([]string, 0, len(q.clients))
	for id := range q.clients {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return ids[0]
}

// SetAccountOrder 由 main 注入账号顺序（config 顺序），确定性兜底。
func (q *Queue) SetAccountOrder(ids []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.order = append([]string(nil), ids...)
}

func New(ctx context.Context, clients map[string]*onebot.Client, interval time.Duration, log *slog.Logger) *Queue {
	q := &Queue{
		clients: clients,
		log:     log.With("component", "send"),
		ctx:     ctx,
		queues:  make(map[string]*chatQueue),
	}
	q.SetInterval(interval)
	return q
}

// SetInterval 热更新发送限频（配置热重载用）。
func (q *Queue) SetInterval(d time.Duration) { q.interval.Store(int64(d)) }

// Depth 返回当前排队中的消息总数（所有会话队列之和）。
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, cq := range q.queues {
		n += len(cq.ch)
	}
	return n
}

// OnSent 每次成功投递后回调（指标采集用；在 worker goroutine 上调用）。
func (q *Queue) SetOnSent(fn func()) { q.onSent = fn }

// Send 把消息排进队列并等待真正发送完成，返回 message_id。
func (q *Queue) Send(ctx context.Context, account, chatType string, chatID int64, segments []onebot.Segment, replyTo int64) (int64, error) {
	if len(segments) == 0 {
		return 0, nil
	}

	// 先看有没有捕获者：测试注入时把消息交给它，不真发
	if holder, _ := q.capture.Load().(captureHolder); holder.fn != nil {
		holder.fn(CapturedMessage{
			Account:  account,
			ChatType: chatType,
			ChatID:   chatID,
			Segments: segments,
			ReplyTo:  replyTo,
		})
		return 0, nil
	}

	// 兜底静音：捕获者不在但还在测试期（比如插件超时后才发消息），一律丢弃
	if q.muted.Load() {
		q.log.Debug("静音中，丢弃发送", "chat", chatType, "id", chatID)
		return 0, nil
	}

	j := job{
		ctx:      ctx,
		chatType: chatType,
		chatID:   chatID,
		segments: segments,
		replyTo:  replyTo,
		done:     make(chan result, 1),
	}

	// **取 worker 和投递在同一把锁里做**。
	// 否则 worker 可能刚判定"队列空"退出，这里把消息塞进一个没人读的 channel，
	// 调用方一直等 r.done —— 表现为"发送超时"。
	// key 必须含账号：同一个群号在不同账号下是两个独立的会话，
	// 共用 worker 会用错 client（后发的走先创建 worker 的那个账号）。
	key := fmt.Sprintf("%s|%s:%d", account, chatType, chatID)
	if !q.enqueue(account, key, j) {
		return 0, fmt.Errorf("发送队列已满（会话 %s:%d），稍后再试", chatType, chatID)
	}

	select {
	case r := <-j.done:
		return r.id, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// SendText 是发纯文本的快捷方法。
func (q *Queue) SendText(ctx context.Context, account, chatType string, chatID int64, text string, replyTo int64) (int64, error) {
	return q.Send(ctx, account, chatType, chatID, []onebot.Segment{onebot.Text(text)}, replyTo)
}

// enqueue 在锁内取/建 worker 并投递，返回是否投递成功。
//
// 队列满时返回 false（调用方报错），**不阻塞等**——
// 持锁阻塞会把所有会话的发送都堵住。
func (q *Queue) enqueue(account, key string, j job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	cq, ok := q.queues[key]
	if !ok {
		cq = &chatQueue{ch: make(chan job, sendQueueDepth)}
		q.queues[key] = cq
		go q.worker(account, key, cq)
	}
	cq.lastUsed.Store(time.Now().Unix())

	select {
	case cq.ch <- j:
		return true
	default:
		return false
	}
}

// sendQueueDepth 是单个会话的发送队列上限。
const sendQueueDepth = 256

// queueIdleTimeout 是发送 worker 的空闲回收时间。
// 长跑时私聊会话多，不回收会让 goroutine 一直涨。
const queueIdleTimeout = 10 * time.Minute

func (q *Queue) worker(account, key string, cq *chatQueue) {
	client := q.clients[account]
	defer q.forgetQueue(account, key, cq)

	idle := time.NewTimer(queueIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return
		case <-idle.C:
			// 锁内确认空 + 摘除，和 Send 的入队互斥
			q.mu.Lock()
			empty := len(cq.ch) == 0 && q.queues[key] == cq
			if empty {
				delete(q.queues, key)
			}
			q.mu.Unlock()
			if empty {
				return
			}
			idle.Reset(queueIdleTimeout)
		case j, ok := <-cq.ch:
			if !ok {
				return
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(queueIdleTimeout)
			// 调用方可能已经超时/取消：这时不该再发。
			// 注意"已经开始发送"之后不能中断——发送是原子的，
			// 半路放弃会留下不确定状态。所以只在**发送前**检查。
			if j.ctx != nil && j.ctx.Err() != nil {
				j.done <- result{err: fmt.Errorf("发送已取消: %w", j.ctx.Err())}
				continue
			}
			id, err := q.deliver(client, account, j)
			if err != nil {
				q.log.Error("发送失败", "chat", key, "err", err)
			} else if q.onSent != nil {
				q.onSent()
			}
			j.done <- result{id: id, err: err}
			if d := time.Duration(q.interval.Load()); d > 0 {
				select {
				case <-time.After(d):
				case <-q.ctx.Done():
					return
				}
			}
		}
	}
}

// forgetQueue 把已经退出的发送 worker 从 map 里移除。
func (q *Queue) forgetQueue(account, key string, cq *chatQueue) {
	q.mu.Lock()
	if cur, ok := q.queues[key]; ok && cur == cq {
		delete(q.queues, key)
	}
	q.mu.Unlock()
}

func (q *Queue) deliver(client *onebot.Client, account string, j job) (int64, error) {
	if client == nil {
		return 0, fmt.Errorf("未知账号 %s", account)
	}

	segments := j.segments
	// 是否引用原消息由插件决定：传了 reply_to 就带引用段，不传就是普通消息。
	if j.replyTo != 0 {
		segments = append([]onebot.Segment{{
			Type: "reply",
			Data: map[string]any{"id": fmt.Sprintf("%d", j.replyTo)},
		}}, segments...)
	}

	var action string
	params := map[string]any{"message": segments}
	if j.chatType == "group" {
		action = "send_group_msg"
		params["group_id"] = j.chatID
	} else {
		action = "send_private_msg"
		params["user_id"] = j.chatID
	}

	ctx, cancel := context.WithTimeout(q.ctx, 15*time.Second)
	defer cancel()

	var out struct {
		MessageID int64 `json:"message_id"`
	}
	if err := client.CallInto(ctx, action, params, &out); err != nil {
		return 0, err
	}
	return out.MessageID, nil
}
