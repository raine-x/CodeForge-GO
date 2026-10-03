package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 预约式滑动窗口：reserve
// ---------------------------------------------------------------------------

// 窗口没满时立刻放行，不该让任何请求等待。
func TestReservePassesThroughUntilFull(t *testing.T) {
	var l rateLimiter
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		if got := l.reserve(base, 5); !got.Equal(base) {
			t.Fatalf("第 %d 次不该等待，实际排到 %v", i+1, got.Sub(base))
		}
	}
}

// 窗口满后，新请求排到**最老那个槽位**滑出窗口之后。
//
// 别指望它被「均匀铺开」成 t+W/rpm：那样 3 个请求在 t=0 发出后又会在
// 20s/40s/60s 各发一个，60s 宽的窗口里就有 6 个 —— 限流失效。
// 而 t=60 处的突发是「每分钟 3 个」**允许**的（半开窗口 (0,60] 只含 3 个）。
// 真正的不变式是 g[i+rpm] − g[i] ≥ W，见下面那条并发用例。
func TestReserveSpreadsRequestsBeyondLimit(t *testing.T) {
	var l rateLimiter
	base := time.Unix(1_700_000_000, 0)
	const rpm = 5
	for i := 0; i < rpm; i++ {
		l.reserve(base, rpm)
	}
	// 第 6 个：最老槽位是 base，要等它滑出窗口。
	got := l.reserve(base, rpm)
	want := base.Add(rpmWindow)
	if !got.Equal(want) {
		t.Errorf("窗口满后第 6 个应排到 %v，实际 %v", want, got)
	}
	// 第 7 个：最老槽位还是 base，但必须严格晚于已排的 6 号，
	// 否则下一轮算 slots[0] 会原地打转。
	got = l.reserve(base, rpm)
	if !got.After(want) {
		t.Errorf("第 7 个必须晚于第 6 个（%v），实际 %v —— 否则会原地打转", want, got)
	}
	if len(l.slots) != rpm {
		t.Errorf("slots 应稳定在 %d 个（弹出最老、追加最新），实际 %d", rpm, len(l.slots))
	}
}

// slots 必须只保留最近 rpm 个：否则长时间运行会无界增长
// （与 AGENTS.md 里 FS.pathLocks「只增不减」同类的坑）。
func TestReserveKeepsSlotsBounded(t *testing.T) {
	var l rateLimiter
	base := time.Unix(1_700_000_000, 0)
	const rpm = 4
	for i := 0; i < 500; i++ {
		l.reserve(base, rpm)
	}
	if len(l.slots) != rpm {
		t.Errorf("slots 应稳定在 %d 个以内，实际 %d", rpm, len(l.slots))
	}
}

// rpm=1 的语义是「每分钟最多一个」，于是相邻两次必须隔满整窗。
// 除以 (rpm-1) 会在 rpm=1 时除零 —— 这条钉住那个分支没被写错。
func TestReserveRPMOneSpacesFullWindow(t *testing.T) {
	var l rateLimiter
	base := time.Unix(1_700_000_000, 0)
	l.reserve(base, 1)
	got := l.reserve(base, 1)
	want := base.Add(rpmWindow)
	if !got.Equal(want) {
		t.Errorf("rpm=1 时第二次应隔满一整个窗口（%v），实际 %v", rpmWindow, got.Sub(base))
	}
}

// rpm=0（不限制）必须**真的**不限：曾实现里它会把时刻也记进 slots，
// 于是长时间不限速后 slots 攒下一堆过期时刻，「最早可发」就算错了。
func TestReserveUnlimitedNeverWaits(t *testing.T) {
	var l rateLimiter
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 1000; i++ {
		if got := l.reserve(base.Add(time.Duration(i)*time.Millisecond), 0); got.Before(base) {
			t.Fatalf("不限速时不该排到过去：%v", got)
		}
	}
	if len(l.slots) != 0 {
		t.Errorf("不限速时不该留下槽位，实际 %d 个", len(l.slots))
	}
}

// 闲置超过窗口后重新放行：否则「一分钟只发 5 个」会在闲置后仍然卡住。
func TestReserveReleasesAfterWindowExpires(t *testing.T) {
	var l rateLimiter
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		l.reserve(base, 5)
	}
	// 窗口刚过时：应该立刻放行。
	after := base.Add(rpmWindow + time.Second)
	if got := l.reserve(after, 5); !got.Equal(after) {
		t.Errorf("窗口过期后应立刻放行，实际排到 %v（延后 %v）", got, got.Sub(after))
	}
}

// ---------------------------------------------------------------------------
// 并发：限流最容易在这里失效
// ---------------------------------------------------------------------------

// 先占位再等待是限流成立的前提。若改成「等到点了再占位」，
// 并发的 N 个请求会同时通过检查、同时发出 —— rpm=5 的一分钟里能真发 50 个。
//
// 断言的是**真正的不变式**，不是「时刻互不相同」：
// rpm=3 时前 3 个请求本来就该拿到同一时刻（同一瞬间来了 3 个，窗口允许 3 个）。
// 真正的约束是「任意 rpm+1 个相邻授予之间至少隔满一个窗口」。
func TestReserveUnderConcurrencyNeverOverruns(t *testing.T) {
	var l rateLimiter
	const rpm = 3
	const n = 20
	base := time.Unix(1_700_000_000, 0)

	var mu sync.Mutex
	var granted []time.Time
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			until := l.reserve(base, rpm)
			mu.Lock()
			granted = append(granted, until)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(granted) != n {
		t.Fatalf("应发出 %d 次授予，实际 %d", n, len(granted))
	}
	sort.Slice(granted, func(i, j int) bool { return granted[i].Before(granted[j]) })

	// 不变式：第 i+1 与第 i-rpm 个之间至少隔一个窗口。
	// 违反它就意味着某个宽度为一分钟的窗口里塞进了超过 rpm 个请求。
	for i := rpm; i < len(granted); i++ {
		if gap := granted[i].Sub(granted[i-rpm]); gap < rpmWindow {
			t.Errorf("第 %d 与第 %d 个授予只隔 %v，少于一个窗口（%v）—— 某窗口内超过了 %d 个",
				i, i-rpm, gap, rpmWindow, rpm)
		}
	}
	// 也不该慢到离谱：20 个请求、每分钟 3 个，最后一个应在第 7 个窗口之前
	//（每波 3 个，第 7 波放最后两个）。上界给一波余量。
	if last := granted[len(granted)-1].Sub(base); last > 7*rpmWindow {
		t.Errorf("铺开过度：最后一个请求被排到 %v 之后（应在 7 个窗口内）", last)
	}
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

// 同一模型 id 必须拿到**同一个**限流器：正式对话与设置页「测试连接」
// 共用配额，正是靠这一点。不同模型则互不干扰。
func TestLimiterRegistrySharesPerModel(t *testing.T) {
	resetLimiters()
	if a, b := limiterFor("m-a"), limiterFor("m-a"); a != b {
		t.Error("同一模型应拿到同一个限流器，否则测试连接与对话各算各的")
	}
	if a, b := limiterFor("m-a"), limiterFor("m-b"); a == b {
		t.Error("不同模型不该共用限流器：一个被限不该拖慢另一个")
	}
	resetLimiters()
}

// 空模型 id 也要返回一个可用的限流器（各限各的），不能 nil。
func TestLimiterForEmptyModelStillUsable(t *testing.T) {
	resetLimiters()
	l := limiterFor("")
	if l == nil {
		t.Fatal("空模型 id 也该返回一个限流器")
	}
	base := time.Unix(1_700_000_000, 0)
	if got := l.reserve(base, 2); !got.Equal(base) {
		t.Errorf("空模型 id 不该影响第一次放行，实际 %v", got)
	}
	resetLimiters()
}

// 闲置条目要被回收，否则「删掉的模型」会在 map 里各留一个槽位。
// 这正是 FS.pathLocks 被记进 AGENTS.md 已知问题的那个形状，别再犯一次。
func TestLimiterRegistrySweepsIdleEntries(t *testing.T) {
	resetLimiters()
	limiterFor("gone")
	limiterMu.Lock()
	before := len(limiterRegistry)
	limiterMu.Unlock()
	if before != 1 {
		t.Fatalf("应登记 1 条，实际 %d", before)
	}

	// 推进 nowFunc 越过 TTL，再取一次别的模型触发清扫。
	restore := nowFunc
	nowFunc = func() time.Time { return restore().Add(limiterTTL + time.Minute) }
	defer func() { nowFunc = restore }()
	limiterFor("fresh")

	limiterMu.Lock()
	after := len(limiterRegistry)
	hasGone := false
	for k := range limiterRegistry {
		if k == "gone" {
			hasGone = true
		}
	}
	limiterMu.Unlock()
	if hasGone {
		t.Error("闲置条目应被回收")
	}
	if after != 1 {
		t.Errorf("回收后应只剩 1 条（fresh），实际 %d", after)
	}
	resetLimiters()
}

// ---------------------------------------------------------------------------
// AcquireSlot：等待必须响应取消
// ---------------------------------------------------------------------------

// 用户点了停止 / 会话被切走时，等待要立刻退出并把 ctx.Err() 交回去。
// 否则一次限流等待能把整个界面挂住 —— 这是加等待时最容易漏的一环。
func TestAcquireSlotHonorsContextCancel(t *testing.T) {
	resetLimiters()
	// 先占满 1 个槽位，让下一个必须等整整一分钟。
	base := time.Unix(1_700_000_000, 0)
	limiterFor("cancel-model").reserve(base, 1)

	restore := nowFunc
	nowFunc = func() time.Time { return base }
	defer func() {
		nowFunc = restore
		resetLimiters()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消：等待应立刻返回而不是睡一分钟
	start := time.Now()
	err := AcquireSlot(ctx, "cancel-model", 1)
	if err == nil {
		t.Fatal("已取消的等待必须返回错误")
	}
	if err != context.Canceled {
		t.Errorf("应原样返回 ctx.Err()（这样 errs 才能归成 KindCanceled），实际 %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("取消后不该真的等待，耗时 %v", d)
	}
}

// rpm<=0 时 AcquireSlot 必须是零成本的空操作（默认无限制的路径不能有开销）。
func TestAcquireSlotNoopWhenUnlimited(t *testing.T) {
	resetLimiters()
	start := time.Now()
	if err := AcquireSlot(context.Background(), "unlimited", 0); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("不限速时不该等待，耗时 %v", d)
	}
}

// ---------------------------------------------------------------------------
// postJSON 接线
// ---------------------------------------------------------------------------

// 端到端：配置了 RPM 时，postJSON 会**真的把请求延后**发出。
// 用 rpm=1 + 连续两次请求验证第二次被推迟，且耗时接近一个窗口量级会被
// 测试超时拖死 —— 所以这里把窗口缩到 60ms（只在这个用例里替换常量不可行，
// 改用「第一次请求先占位、第二次只验证排到未来」的方式）。
func TestPostJSONRespectsRateLimitAcrossCalls(t *testing.T) {
	resetLimiters()
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	gate := &rateLimiter{}
	base := time.Unix(1_700_000_000, 0)
	restore := nowFunc
	// nowFunc 固定住，于是两次 reserve 落在同一逻辑时刻：
	// 第一次拿到 base，第二次必须被排到 base+1min。
	nowFunc = func() time.Time { return base }
	defer func() {
		nowFunc = restore
		resetLimiters()
	}()

	// 第一次：立即发。
	if _, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, RetryPolicy{MaxAttempts: 1}, gate, 1); err != nil {
		t.Fatalf("第一次请求应成功: %v", err)
	}
	mu.Lock()
	if hits != 1 {
		t.Fatalf("第一次应真的发出 1 次，实际 %d", hits)
	}
	mu.Unlock()

	// 第二次：会被限流器排到 base+1min，而 ctx 只给 50ms —— 于是应当
	// 因超时/取消返回错误，且**服务端一次都没被打到**。
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := postJSON(ctx, srv.URL, nil, map[string]any{}, RetryPolicy{MaxAttempts: 1}, gate, 1); err == nil {
		t.Fatal("超出 RPM 的第二次请求应因等待而未发出")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("被限流的那次不该真的打到上游，实际累计 %d 次", hits)
	}
}

// 未配置 RPM 时不受影响：不传限流器（nil）或 rpm=0，请求必须立刻发出。
// 这是「默认无限制」的回归保护 —— 加节流不能反过来拖慢所有既有用户。
func TestPostJSONUnlimitedByDefault(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for i := 0; i < 5; i++ {
		if _, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, RetryPolicy{MaxAttempts: 1}, nil, 0); err != nil {
			t.Fatalf("第 %d 次应成功: %v", i+1, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 5 {
		t.Errorf("默认不限速时 5 次请求应全部发出，实际 %d", hits)
	}
}
