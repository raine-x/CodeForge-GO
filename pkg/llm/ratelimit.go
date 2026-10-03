// ratelimit.go 实现按模型的客户端 RPM 节流（发请求**之前**就错开，从不撞上游 429）。
//
// 为什么不用「撞了 429 再退避」当限流
//
//	429 说明上游已经嫌我们快了，而那一刻配额已经被消耗、这一轮请求白等。
//	免费额度 / 共享密钥 / 有明确配额公告的场合，用户**事先就知道**自己每分钟
//	只能发几个 —— 那就该事先错开，而不是每次都去撞墙。postJSON 里 429 的
//	退避重试是「事后补救」，与本文件的「事前预防」是两套机制，都留着。
//
// 为什么是预约式而不是排队式
//
//	朴素做法是「进队 → 排队 → 一个个发」。问题：ReAct 循环里多个请求可能
//	**并发**发起（多会话同时跑），排队要求它们各自占一个 goroutine 干等，
//	而这些等待发生在同一个请求的生命周期里，取消/超时处理会很别扭。
//
//	这里改成**预约**：每个请求进来就算出「你最早可以在什么时刻发」，
//	立刻把这个时刻占住，然后各自 sleep 到自己的时刻。
//
//	于是 rpm=5、5 个请求同时到达时，它们被分配到 t、t+12s、t+24s…，
//	互不干扰、严格不越线、也不需要任何队列。等待期间用户照样能点停止
//	（sleep 走 ctx）。
//
// 为什么是严格滑动窗口而不是令牌桶
//
//	令牌桶允许短时突发，而**突发正是撞 429 的原因** —— 上游的限流器
//	通常也是滑动窗口（甚至更严）。宁可慢，不可越线。
package llm

import (
	"context"
	"sync"
	"time"

	"codeforge/pkg/logx"
)

// rpmWindow 是 RPM 的计量窗口。固定 1 分钟（题面即「每分钟」）。
//
// 不做成可配置项：窗口跟着上限走会让语义变得难解释（限 5 次/5 分钟？
// 还是 1 次/分钟？），而用户实际遇到的配额说明几乎都按分钟表述。
const rpmWindow = time.Minute

// rateLimiter 是单个模型的时间戳环，维护最近一分钟内「已预定发送时刻」。
//
// 按模型实例独立持有：不同模型的配额互不相干，一个被限也不该拖慢另一个。
// 状态只在进程内存里 —— 重建 provider 时计数从零开始，这没问题：
// 节流是为了**减少**撞 429，不是安全边界，重启后重新计一分钟无害。
type rateLimiter struct {
	mu sync.Mutex
	// slots 是升序排列的预定发送时刻。升序是预约式算法的前提：
	// 「最早能发」永远是 slots[0]。
	slots []time.Time
}

// reserve 为一次请求预订发送时刻，返回「最早可以发送的时刻」。
//
// 调用方负责在它返回的时刻之前不要真的发出去（见 postJSON 的等待逻辑）。
// 这个「预订」是限流成立的关键：**先占位再等待**。
// 若改成「等到点了再占位」，并发的 N 个请求会同时通过检查、同时发出，
// 于是 rpm=5 的一分钟里可能真的发出 50 个 —— 限流形同虚设。
//
// 约束的准确形式：把授予时刻排好序后，第 i+rpm 个与第 i 个之间**至少隔
// 满一个窗口**（g[i+rpm] − g[i] ≥ W）。它等价于「任意一分钟内至多 rpm 个」
// （半开窗口计数）。于是槽位满时，新请求必须排到**最老那个**槽位滑出窗口
// 的时刻。
//
// ⚠️ 别把步长改成 W/rpm 去「均匀铺开」。那看着更温和，实际会破坏上面的
// 不变式：3 个请求都在 t=0 发出后，又在 20s/40s/60s 各发一个，于是宽度
// 为 60s 的窗口里出现了 6 个。而 t=60 处那一下突发**恰恰是「每分钟 3 个」
// 允许的**（窗口 (0,60] 只含 3 个），上游的限流器同样这么算。
// 曾把这个算法写错过一次，测试（TestReserveUnderConcurrencyNeverOverruns）
// 抓出来了 —— 它断言的正是 g[i+rpm] − g[i] ≥ W 这条。
//
// now 显式传入而不是内部取 time.Now()：让「窗口边界」在测试里可控，
// 不用真的睡一分钟。
func (l *rateLimiter) reserve(now time.Time, rpm int) time.Time {
	// 整个函数体必须在同一把锁里：读 slots、判满、算时刻、追加 ——
	// 这是「先占位再等待」成立的前提。一旦拆开，两个并发请求会各自看到
	// 「还没满」而双双放行，限流当场失效。
	//
	// 曾漏掉这把锁：串行调用（含 20000 轮乱序压力）完全正常，
	// 只有真并发下约 60% 概率违反不变式 —— 那种 bug 靠肉眼看代码是看不出来的，
	// 必须靠并发用例 + go test -race 才暴露（见 TestReserveUnderConcurrencyNeverOverruns）。
	l.mu.Lock()
	defer l.mu.Unlock()
	if rpm <= 0 {
		// 0 / 负数 = 不限制。仍要清理陈旧槽位，否则长时间不限速后
		// slots 会攒下一堆过期时刻，让「最早可发时刻」算错。
		l.trimLocked(now)
		return now
	}
	l.trimLocked(now)
	// **入表时刻必须不早于最后一个槽位**，否则 slots 会失去升序性。
	//
	// 真实并发里 now 会乱序：goroutine A 取到 100ms、B 取到 200ms，
	// 而 B 先跑完先入表 —— 于是 slots 变成 [200ms, 100ms]。
	// trimLocked 靠升序才能从头扫（它是 FIFO），一乱序就漏掉过期项，
	// 算出来的 slots[0] 也未必是最老的，限流随即失效。
	//
	// ⚠️ 钳制只能作用于**入表用的时刻**，绝不能回头去喂 trimLocked：
	// 那里若拿到被抬到未来的 now，会把「当前真实时间之前」的槽位当过期清掉，
	// 于是 slots 越钳越少、限流越钳越松（实测 rpm=5 被清到只剩 2 个）。
	//
	// 方向是保守的：把请求算作发生在已排定的更晚时刻，只会让我们等更久。
	base := now
	if n := len(l.slots); n > 0 && l.slots[n-1].After(base) {
		base = l.slots[n-1]
	}
	if len(l.slots) < rpm {
		l.slots = append(l.slots, base)
		return base
	}
	// 槽位已满：排到最老的那个滑出窗口之后。
	//
	// slots 只保留最近 rpm 个，所以 slots[0] 恰好是「第 n−rpm 次授予」，
	// 于是 new = g[n−rpm] + W，正是 g[i+rpm] − g[i] ≥ W 这条不变式。
	//
	// 顺带保证严格递增（同一时刻可以容纳 rpm 个，但**槽位**要能区分开，
	// 否则下一轮算 slots[0] 会一直读到同一个值而原地打转）。
	earliest := l.slots[0].Add(rpmWindow)
	if last := l.slots[len(l.slots)-1]; !earliest.After(last) {
		earliest = last.Add(time.Nanosecond)
	}
	l.slots = append(l.slots[1:], earliest)
	return earliest
}

// trimLocked 丢弃窗口外的时刻（调用方须已持锁）。
//
// slots 是 FIFO 且升序，过期的必然在前缀，所以从头扫一遍即可；
// 用 append(slots[:0], slots[i:]...) 原地左移而不重新分配。
func (l *rateLimiter) trimLocked(now time.Time) {
	cut := now.Add(-rpmWindow)
	i := 0
	for i < len(l.slots) && !l.slots[i].After(cut) {
		i++
	}
	if i == 0 {
		return
	}
	if i == len(l.slots) {
		l.slots = l.slots[:0]
		return
	}
	l.slots = append(l.slots[:0], l.slots[i:]...)
}

// waitUntil 等待到指定时刻，期间响应 ctx 取消（用户点停止 / 会话被切走）。
//
// 返回的 error 是 ctx.Err()，由调用方原样返回 —— 归类成 KindCanceled，
// 于是界面显示「已取消」而不是「失败」。
func waitUntil(ctx context.Context, until time.Time) error {
	d := until.Sub(nowFunc())
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// nowFunc 是取当前时刻的间接层，测试里可替换以避免真的睡眠。
var nowFunc = time.Now

// ---------------------------------------------------------------------------
// 按模型 id 的注册表
//
// 为什么需要它：设置页的「测试连接」能测**任意**模型（不只是当前那个），
// 但它必须和正式对话**共用同一个计数器** —— 用户连点十次「测试连接」
// 打满的是同一把密钥的额度，之后的对话就该被限了。
//
// provider 实例自带一个 limiter 是不够的：那只能让「当前模型」的
// 正式对话与测试共享，换成测别的模型就各算各的。
//
// 为什么不会无界增长：键是**模型 id**，而模型 id 的数量由用户的模型库
// 决定（配置量），不是「碰过的路径数」那种随使用无限增长的键
// （那正是 FS.pathLocks 被记进已知问题的原因）。这里仍然加了惰性清理，
// 把「删掉的模型」留下的槽位也一并回收掉，不让 map 只增不减。
// ---------------------------------------------------------------------------

// limiterTTL 是注册表条目的闲置存活时长。
//
// 清理掉一个 limiter 的唯一后果是它的计数归零，而归零后的第一次请求
// 至多多发一个（正好卡在窗口边界），之后立刻恢复正常节流 —— 拿一个
// 边角场景的正确性换 map 有界，这个交换是划算的。
const limiterTTL = time.Hour

// limiterEntry 是注册表里的一项：limiter 本体 + 最近一次被使用的时间。
type limiterEntry struct {
	limiter  *rateLimiter
	lastUsed time.Time
}

var (
	limiterMu       sync.Mutex
	limiterRegistry = map[string]*limiterEntry{}
)

// limiterFor 返回该模型的限流器，不存在则新建。
//
// model 为空串时返回一个**独立**的限流器：拿不到模型 id 就没有共享的语义，
// 此时各调用方各限各的也比什么都不限强。
func limiterFor(model string) *rateLimiter {
	limiterMu.Lock()
	defer limiterMu.Unlock()
	now := nowFunc()
	sweepLimitersLocked(now)
	if model == "" {
		return &rateLimiter{}
	}
	e, ok := limiterRegistry[model]
	if !ok {
		e = &limiterEntry{limiter: &rateLimiter{}}
		limiterRegistry[model] = e
	}
	e.lastUsed = now
	return e.limiter
}

// sweepLimitersLocked 回收闲置超过 limiterTTL 的条目（调用方须已持锁）。
//
// 惰性清扫：不起后台 goroutine（项目要求低内存、跨平台静态编译，
// 多一个常驻 goroutine 就多一处需要在 Windows/Android 上验证的唤醒行为）。
// 挂在每次 limiterFor 上，摊销成本可以忽略。
func sweepLimitersLocked(now time.Time) {
	for k, e := range limiterRegistry {
		if now.Sub(e.lastUsed) > limiterTTL {
			delete(limiterRegistry, k)
		}
	}
}

// resetLimiters 清空注册表，仅供测试使用。
func resetLimiters() {
	limiterMu.Lock()
	defer limiterMu.Unlock()
	limiterRegistry = map[string]*limiterEntry{}
}

// AcquireSlot 为一次 HTTP 尝试预订发送时刻，并在必要时等到那一刻。
//
// 这是给**走自己 HTTP 路径**的调用方用的（目前只有设置页「测试连接」——
// 它不经过 postJSON）。走 postJSON 的对话请求不需要它，那边直接调
// reserve + waitUntil，因为它要逐次重试分别占位。
//
// 返回的 error 是 ctx.Err()（用户取消 / 超时），由调用方原样处理。
//
// ⚠️ 调用方**必须**在返回后立刻真的发出这一次请求。若拿到 nil 就把请求
// 丢掉，槽位就白占了 —— 下游会被无谓地推迟整整一个窗口。
func AcquireSlot(ctx context.Context, model string, rpm int) error {
	if rpm <= 0 {
		return nil
	}
	until := limiterFor(model).reserve(nowFunc(), rpm)
	if wait := until.Sub(nowFunc()); wait > 0 {
		logx.Infof("已达每分钟 %d 个请求的上限，本次延后 %s 发出（模型 %s 节流）", rpm, wait.Round(time.Second), model)
		return waitUntil(ctx, until)
	}
	return nil
}
