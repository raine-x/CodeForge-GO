package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"codeforge/pkg/agent"
	"codeforge/pkg/errs"
	"codeforge/pkg/platform"
	"codeforge/pkg/tools"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return strings.Contains(origin, "127.0.0.1") ||
			strings.Contains(origin, "localhost") ||
			strings.Contains(origin, "[::1]")
	},
}

// wsMessage 是客户端上行消息。
type wsMessage struct {
	Attachments []string `json:"attachments"`
	Type        string   `json:"type"`
	SessionID   string   `json:"session_id"`
	Text        string   `json:"text"`
	Title       string   `json:"title"`
	ApprovalID  string   `json:"approval_id"`
	Approved    bool     `json:"approved"`
	Thinking    string   `json:"thinking"` // 思考强度：low/medium/high
	Hidden      bool     `json:"hidden"`   // 页面是否不可见（visibility 消息携带）

	// ---- 编辑重发 / 回滚 ----
	// Back 是「距最后一条用户消息的距离」（0 = 最后一条），仅 edit_user_message 使用。
	// 用相对位置而非绝对下标：前端拿到的是渲染顺序，与服务端 Messages 下标不是一回事，
	// 用相对距离可以避免两者错位时改错消息。
	Back int `json:"back"`
	// ToStep 是回滚目标步骤（rewind 使用：回退到该步**之前**）。
	ToStep int `json:"to_step"`
	// RollbackFiles 表示编辑重发时是否同时回退该消息之后的文件改动（缺省 true）。
	RollbackFiles *bool `json:"rollback_files"`
}

// wsClient 表示一个浏览器 WebSocket 连接。
type wsClient struct {
	srv      *Server
	conn     *websocket.Conn
	writeMu  sync.Mutex
	approver *wsApprover

	cancelMu sync.Mutex
	cancel   context.CancelFunc
	// runSeq 是「第几轮」的代号：每起一轮 +1，收尾时比对代号，
	// 只有仍是当前轮的 goroutine 才允许改前端的运行态。
	// 上一轮被打断后是异步收尾的，它那句迟到的 idle 若照发，
	// 就会把紧接着起来的新一轮标成「已空闲」（按钮恢复、思考条消失，
	// 看起来像任务凭空停了）。
	runSeq int

	// 任务完成通知的状态：页面可见性 + 节流时间戳。
	notifyMu        sync.Mutex
	pageHidden      bool      // 页面是否不可见（缺省 true：未知时按「不可见」处理，保持通知可用）
	visibilityKnown bool      // 前端是否已上报过可见性
	lastNotify      time.Time // 上次发送通知的时间（节流用）
}

func (c *wsClient) send(v any) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.WriteJSON(v)
}

// sendErr 通过 WS 下发一条**可读**的错误提示。
//
// 与 writeErr 同一口径：底层错误先经 errs.FriendlyOr 翻译成「成因 + 建议」，
// 已经是人话的业务错误则原样透出。
//
// 此前这里直接发 err.Error()，用户会在对话里看到 "unexpected EOF"
// 这种标准库原文 —— 既不知道发生了什么，也不知道能不能重试（2026-09-21 实测）。
func (c *wsClient) sendErr(action string, err error) {
	c.send(map[string]any{"type": "error", "error": errs.FriendlyOr(action, err)})
}

// handleWS 处理 WebSocket 连接：接收用户消息、推送事件流、处理 HITL 决策。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] 升级失败: %v", err)
		return
	}
	client := &wsClient{srv: s, conn: conn, pageHidden: true}
	client.approver = &wsApprover{client: client, pending: map[string]chan bool{}}

	defer func() {
		client.stop()
		_ = conn.Close()
	}()

	client.send(map[string]any{
		"type":     "ready",
		"sessions": s.agent.History().List("", false), // 侧栏按工作区分组展示全部
		"tools":    s.registry.Names(),
		"config":   s.configView(),
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		client.dispatch(msg)
	}
}

func (c *wsClient) dispatch(msg wsMessage) {
	ws := c.srv.agent.WorkDir() // 会话/记忆按工作区隔离
	switch msg.Type {
	case "user_message":
		c.startUserMessage(msg)

	case "steer":
		// 运行中追加指令（转向）：交给正在跑的循环在下一个步骤边界并入，
		// 已产生的工具结果全部保留在上下文里。
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			return
		}
		switch c.srv.agent.Steer(msg.SessionID, text) {
		case agent.SteerQueued:
			c.send(map[string]any{"type": "steer_queued", "session_id": msg.SessionID})
		case agent.SteerFull:
			c.send(map[string]any{"type": "error", "error": "转向指令排队已满，请等当前任务消化完再发"})
		default:
			// 没有运行中的循环：指令不该压在队列里等一个不会来的步骤边界，
			// 按普通用户消息照常起一轮。
			msg.Text = text
			c.startUserMessage(msg)
		}

	case "regenerate":
		// 重新生成：回退会话历史中最后一轮回复，基于同一条用户提问重跑。
		if msg.SessionID == "" {
			return
		}
		// 打断旧任务必须**同步**发生在启动新 goroutine 之前（dispatch 串行）：
		// 放进 run() 里再 stop，两条快速连续的消息会让两个 goroutine 都先跑过
		// stop()（cancel 还是 nil），随后第二个覆盖第一个的 cancel ——
		// 第一个循环从此无法打断，且两个循环并发写同一会话。
		c.stop()
		go c.run(msg.SessionID, msg.Thinking, "重新生成", "", func(ctx context.Context, emit func(agent.Event)) error {
			return c.srv.agent.Regenerate(ctx, msg.SessionID, emit)
		})

	case "edit_user_message":
		// 编辑重发：截断到目标用户消息 → 回退压缩态 → 换文本重跑。
		// 顺带（默认）把该消息之后的文件改动一并回退，否则模型上下文说「文件是 A」，
		// 而磁盘上还留着 B —— 重新生成的回答必然建立在错误的现状上。
		if msg.SessionID == "" || strings.TrimSpace(msg.Text) == "" {
			return
		}
		c.stop() // 同 regenerate：先同步打断，再启动新 goroutine
		go c.run(msg.SessionID, msg.Thinking, "编辑重发", msg.Text, func(ctx context.Context, emit func(agent.Event)) error {
			rollback := msg.RollbackFiles == nil || *msg.RollbackFiles
			if rollback {
				// 检查点按「步骤」归属，而消息截断按「消息」归属，两者没有直接映射。
				// 参见 agent.RewindAfterEdit：无法定位目标消息时返回 nil（跳过文件回滚，
				// 只截断对话），可定位时按其保守口径回退。
				res, err := c.srv.agent.RewindAfterEdit(msg.SessionID, msg.Back)
				if err != nil {
					log.Printf("[edit] 会话=%s 文件回退失败（已跳过，仅重跑对话）：%v", msg.SessionID, err)
				} else if res != nil && len(res.Paths) > 0 {
					emit(agent.Event{Type: agent.EventRewind, Text: "已回退文件改动", Rewind: res})
				}
			}
			// edit 帧由 RerunFrom 自己在「截断之后、跑循环之前」发出，
			// 这里不能再补：晚了会覆盖刚流出的新回复（见 RerunFrom 注释）。
			_, editErr := c.srv.agent.EditAndResend(ctx, msg.SessionID, msg.Back, msg.Text, emit)
			return editErr
		})

	case "retry":
		// 断点重试：最后一轮没跑完（打断 / 报错 / 刷新页面），保留用户原话，
		// 只把其后未完成的回复与工具结果截掉重跑。与 edit_user_message 同一套
		// 截断 + 文件回退逻辑，唯一区别是不改写用户消息。
		if msg.SessionID == "" {
			return
		}
		back := msg.Back
		if back < 0 {
			back = 0
		}
		c.stop() // 同 regenerate：先同步打断，再启动新 goroutine
		go c.run(msg.SessionID, msg.Thinking, "断点重试", "", func(ctx context.Context, emit func(agent.Event)) error {
			rollback := msg.RollbackFiles == nil || *msg.RollbackFiles
			if rollback {
				// 检查点按「步骤」归属，而消息截断按「消息」归属，两者没有直接映射。
				// 参见 agent.RewindAfterEdit：无法定位目标消息时返回 nil（跳过文件回滚，
				// 只截断对话），可定位时按其保守口径回退。
				res, err := c.srv.agent.RewindAfterEdit(msg.SessionID, back)
				if err != nil {
					log.Printf("[retry] 会话=%s 文件回退失败（已跳过，仅重跑对话）：%v", msg.SessionID, err)
				} else if res != nil && len(res.Paths) > 0 {
					emit(agent.Event{Type: agent.EventRewind, Text: "已回退文件改动", Rewind: res})
				}
			}
			// edit 帧由 RerunFrom 自己在「截断之后、跑循环之前」发出，这里不能再补。
			_, _, rerunErr := c.srv.agent.RerunFrom(ctx, msg.SessionID, back, "", emit)
			return rerunErr
		})

	case "rewind":
		// 手动回滚：把工作区文件恢复到第 ToStep 步之前（不动对话历史）。
		if msg.SessionID == "" {
			return
		}
		res, err := c.srv.agent.RewindFiles(msg.SessionID, msg.ToStep)
		if err != nil {
			c.sendErr("回退文件改动", err)
			return
		}
		c.send(map[string]any{"type": "rewind", "session_id": msg.SessionID, "result": res})
		c.send(c.srv.checkpointEvent(msg.SessionID))

	case "checkpoints":
		// 前端打开回滚菜单时拉一次最新检查点列表。
		c.send(c.srv.checkpointEvent(msg.SessionID))

	case "new_session":
		sess, err := c.srv.agent.History().Create(ws, msg.Title)
		if err != nil {
			c.sendErr("创建会话", err)
			return
		}
		c.send(map[string]any{"type": "session", "session_id": sess.ID, "title": sess.Title})
		c.send(map[string]any{"type": "sessions", "items": c.srv.agent.History().List("", false)})
		c.send(c.srv.contextUsage(sess.ID))
		c.send(c.srv.todoEvent(sess.ID)) // 新会话无清单：前端清空任务列
		c.send(c.srv.checkpointEvent(sess.ID))

	case "list_sessions":
		c.send(map[string]any{"type": "sessions", "items": c.srv.agent.History().List("", false)})

	case "load_session":
		// 切换会话：回放该会话全部历史消息（前端用渲染原语重建聊天列）。
		sess, ok := c.srv.agent.History().Get(msg.SessionID)
		if !ok {
			c.send(map[string]any{"type": "error", "error": "会话不存在: " + msg.SessionID})
			return
		}
		// 内存缓存换成用户想继续的那条（后续 user_message 直接续聊）
		c.send(c.srv.historyEvent(sess.ID))
		c.send(c.srv.contextUsage(sess.ID))
		c.send(c.srv.todoEvent(sess.ID))       // 切会话：回放该会话的任务清单
		c.send(c.srv.checkpointEvent(sess.ID)) // 回放可编辑白名单（编辑按钮的数据源）

	case "hitl_decision":
		c.approver.resolve(msg.ApprovalID, msg.Approved)

	case "context":
		// 前端打开上下文进度条明细时主动拉一次最新占用（会话可能刚被切换）。
		c.send(c.srv.contextUsage(msg.SessionID))

	case "visibility":
		// 前端上报页面可见性：窗口在前台可见时，任务完成不弹系统通知（不打扰）。
		c.setVisibility(msg.Hidden)

	case "cancel":
		// 打断当前任务：由 run() 统一收尾发送 idle；无任务时兜底复位前端状态。
		if !c.stop() {
			c.send(map[string]any{"type": "idle", "reason": "cancelled"})
		}
	}
}

// startUserMessage 起一轮正常的用户对话。
//
// 附件读取放在 run() 的 goroutine 里做：浏览器上传的暂存文件可能是一整个目录，
// 留在读循环里会把后续消息（包括打断）一起堵死。
func (c *wsClient) startUserMessage(msg wsMessage) {
	if strings.TrimSpace(msg.Text) == "" && len(msg.Attachments) == 0 {
		return
	}
	ws := c.srv.agent.WorkDir() // 会话按工作区隔离
	sessionID := msg.SessionID
	if sessionID == "" {
		sess, err := c.srv.agent.History().Create(ws, "")
		if err != nil {
			c.sendErr("创建会话", err)
			return
		}
		sessionID = sess.ID
	}
	c.stop() // 同步打断旧任务后再起新轮（否则两条连发消息会让两轮并存）
	go c.run(sessionID, msg.Thinking, "用户消息", msg.Text, func(ctx context.Context, emit func(agent.Event)) error {
		images, err := readAttachmentImages(ws, msg.Attachments)
		if err != nil {
			return err
		}
		return c.srv.agent.RunWithImages(ctx, sessionID, msg.Text, images, emit)
	})
}

// run 在独立 goroutine 中执行一轮 Agent 交互（正常对话或重新生成）。
// trigger/label 仅用于日志：明确「这一轮是谁、以什么方式触发的」，
// 便于事后排查「我没操作，怎么跑了一轮」这类问题。
func (c *wsClient) run(sessionID, thinking, trigger, label string, agentFn func(ctx context.Context, emit func(agent.Event)) error) {
	c.stop()

	log.Printf("[run] 会话=%s 触发=%s 输入=%q", sessionID, trigger, clipText(label, 80))

	ctx, cancel := context.WithCancel(context.Background())
	c.cancelMu.Lock()
	c.cancel = cancel
	c.runSeq++
	mySeq := c.runSeq
	c.cancelMu.Unlock()
	defer cancel()

	// owns 判断这一轮是否仍是「当前轮」：被打断的旧轮在收尾时要用它挡一遍，
	// 否则迟到的 idle / error 会覆盖新一轮的运行态。
	owns := func() bool {
		c.cancelMu.Lock()
		defer c.cancelMu.Unlock()
		return c.runSeq == mySeq
	}

	// 将本连接的审批器注入 context，使 HITL 请求路由到当前浏览器。
	ctx = tools.WithApprover(ctx, c.approver)
	// 记录审批所属会话：用户切走后审批卡片仍能标注来源。
	c.approver.setSession(sessionID)
	// 注入子智能体事件转发槽：SubagentRunner 里的实时进度经此推给前端。
	ctx = tools.WithSubagentSink(ctx, func(sev tools.SubagentEvent) {
		if ctx.Err() != nil {
			return // 主任务已取消：不再转发子智能体事件
		}
		c.send(map[string]any{"type": "subagent", "subagent": sev})
	})
	// 注入任务清单刷新槽：todo_write 落库后向前端实时推送最新清单。
	ctx = tools.WithTodoSink(ctx, func(sid string) {
		if ctx.Err() != nil {
			return
		}
		c.send(c.srv.todoEvent(sid))
	})
	// 注入思考强度（low/medium/high），由 LLM 适配器转换为厂商参数。
	if thinking != "" {
		ctx = agent.WithThinking(ctx, thinking)
	}

	// 主动打断引发的中间层错误不作为故障下发（前端点击 ▶ 打断属正常操作）。
	//
	// 这里顺带**实时刷新上下文占用**：此前只在「新建 / 切换会话」与「本轮 idle」
	// 时下发，轮次进行中进度条一直不动，跑完才「跳」一下 —— 用户看不到上下文
	// 正在被消耗，也就无从预判什么时候会触发压缩（2026-09-21 反馈）。
	//
	// ⚠️ 能在这里安全读取会话：emit 是 agent 在**自己的 goroutine 里同步调用**的，
	// 与它修改 sess.Messages 是同一个 goroutine，不存在数据竞争。
	// 若改成另起 goroutine 去轮询，就必须先给 Session 加锁。
	emit := func(ev agent.Event) {
		if ev.Type == agent.EventError && ctx.Err() != nil {
			return
		}
		// 历史刚被截断（编辑重发 / 断点重试）：除了转发这一帧，还要下发一份
		// **权威快照**，让前端按库里的样子重建视图。
		//
		// 为什么不能只靠前端自己删：截断点由服务端算（按「最近 N 条纯文本提问」定位），
		// 前端只能靠 DOM 里的 .msg-user 倒着数，两边口径会漂；而且前端「清空整列只留
		// 这一条提问」的旧做法会把**编辑点之前的历史**也一起抹掉，看起来就像记录丢了
		//（2026-09-22 反馈）。给快照就没有猜的余地。
		if ev.Type == agent.EventEdit {
			c.send(ev)
			c.send(c.srv.historyEvent(sessionID))
			return
		}
		c.send(ev)
		if pushesContextOn(ev.Type) {
			c.send(c.srv.contextUsage(sessionID))
		}
	}

	c.send(map[string]any{"type": "session", "session_id": sessionID})
	c.send(map[string]any{"type": "busy"})

	runErr := agentFn(ctx, emit)
	if !owns() {
		// 已被新一轮取代（用户打断后立刻又发了话）：这一轮的收尾全部作废，
		// 由接管它的那一轮负责发 idle / 刷新占用 / 通知。
		log.Printf("[run] 会话=%s 本轮已被更新的轮次取代，跳过收尾事件", sessionID)
		return
	}
	if runErr != nil && ctx.Err() == nil {
		c.send(map[string]any{"type": "error", "error": runErr.Error()})
	}
	c.send(map[string]any{"type": "idle"})
	c.send(map[string]any{"type": "sessions", "items": c.srv.agent.History().List("", false)})
	// 本轮结束后刷新上下文占用：进度条要跟着对话一起长。
	c.send(c.srv.contextUsage(sessionID))
	// 刷新可编辑白名单：又多了几条用户消息，编辑按钮该跟着往后挪。
	c.send(c.srv.checkpointEvent(sessionID))

	// 任务结束后发送系统通知。通知失败只记日志，不影响对话结果；主动取消不算完成通知。
	if ctx.Err() == nil {
		c.notifyDone(sessionID, runErr)
	}
}

// notifyDone 在任务结束后发送「任务完成 / 失败」系统通知。
//
// 发送前依次判断（任一不满足即静默跳过，只记日志）：
//  1. 配置开关（notify.enabled，写 false 完全关闭）；
//  2. 页面可见性 —— 窗口在前台可见（用户正看着）时不打扰；
//  3. 节流 —— 距上次通知不足 notify.min_interval_ms 时不重复打扰。
func (c *wsClient) notifyDone(sessionID string, runErr error) {
	now := time.Now()

	c.notifyMu.Lock()
	reason := notifySkipReason(c.srv.cfg.NotifyEnabled(), c.visibilityKnown, c.pageHidden,
		c.lastNotify, now, c.srv.cfg.NotifyMinInterval())
	if reason == "" {
		c.lastNotify = now
	}
	c.notifyMu.Unlock()
	if reason != "" {
		log.Printf("[notify] 跳过系统通知（会话=%s）：%s", sessionID, reason)
		return
	}

	title, message := "CodeForge", "任务已完成"
	if label := c.sessionLabel(sessionID); label != "" {
		message = "任务已完成：" + label
	}
	if runErr != nil {
		title, message = "CodeForge", "任务执行失败："+clipText(runErr.Error(), 120)
	}
	log.Printf("[notify] 发送系统通知（会话=%s）：%s", sessionID, message)
	go func() {
		if err := platform.Notify(title, message); err != nil {
			log.Printf("系统通知发送失败（已忽略）：%v", err)
		}
	}()
}

// notifySkipReason 返回「不发送通知」的原因；返回空串表示应当发送。
// 拆成返回原因（而非布尔）是为了在日志里明确写出被哪一道闸挡住 ——
// 用户反馈过「我什么都没干，怎么就通知任务完成了」，日志必须能自证原因。
//
//   - enabled 为 false：整体关闭；
//   - hasVisibility 且页面可见（!pageHidden）：用户正看着，不打扰；
//   - last 距 now 不足 minInterval：节流，避免连续任务刷屏。
func notifySkipReason(enabled, hasVisibility, pageHidden bool, last, now time.Time, minInterval time.Duration) string {
	if !enabled {
		return "配置已关闭系统通知（notify.enabled=false）"
	}
	if hasVisibility && !pageHidden {
		return "页面在前台可见，无需提醒"
	}
	if !last.IsZero() && now.Sub(last) < minInterval {
		return "处于节流窗口（距上次通知不足 " + minInterval.String() + "）"
	}
	return ""
}

// shouldNotify 判断是否应发送任务完成通知（纯函数，便于测试）。
func shouldNotify(enabled, hasVisibility, pageHidden bool, last, now time.Time, minInterval time.Duration) bool {
	return notifySkipReason(enabled, hasVisibility, pageHidden, last, now, minInterval) == ""
}

// setVisibility 记录前端上报的页面可见性。
func (c *wsClient) setVisibility(hidden bool) {
	c.notifyMu.Lock()
	c.pageHidden = hidden
	c.visibilityKnown = true
	c.notifyMu.Unlock()
}

// sessionLabel 返回会话标题（供通知文案使用），无标题时返回空串。
func (c *wsClient) sessionLabel(sessionID string) string {
	sess, ok := c.srv.agent.History().Get(sessionID)
	if !ok {
		return ""
	}
	// 走快照：本函数由通知路径调用，与运行中的循环 / 设置页改名并发。
	return clipText(strings.TrimSpace(sess.TitleSnapshot()), 40)
}

// contextUsage 汇总指定会话的上下文占用，供前端底部进度条展示。
//
// 口径完全交给 agent.ContextStat（进度条与自动压缩共用同一个函数），
// 否则进度条会骗人：显示 80% 而实际已经触发压缩，或反过来永不触发。
//
// 语义：
//   - used   = 实际送模的估算 tokens（已用摘要替换掉的旧历史不再计入）；
//   - raw    = 会话历史原文的估算 tokens（用户仍能在界面里看到全部内容）；
//   - budget = 自动压缩的触发线（= (模型窗口 − 输出预留) × 95%，窗口未知时
//     回退 agent.context_token_budget）；
//   - 进度条 100% 的含义就是「下一步请求前会触发摘要压缩」，是有意义的阈值。
//
// percent 保留一位小数且**不封顶**：>100% 表示已越过压缩线（over_budget=true），
// 前端画条时自行夹到 100%，明细里照实显示。
// pushesContextOn 报告某类 agent 事件之后是否要顺带刷新一次上下文占用。
//
// 选的这几类，覆盖「占用会变化」的全部时机：
//   - user：用户刚发的话进了历史 —— 本轮第一次增长；
//   - step：每个步骤边界，上一轮的回复与工具结果都进了历史；
//   - tool_result：**占用增长的主要来源** —— 读文件、跑命令的输出动辄上万 token，
//     用户最需要在这里看到进度条动起来；
//   - compress：压缩刚发生，占用会**回落**，必须立刻反映，否则进度条会一直
//     停在 100% 以上，看起来像坏了。
//
// 刻意不选 delta 类事件（text_delta / reasoning_delta）：它们每秒几十上百条，
// 每次都重算一遍全历史的 EstimateTokens 会白白烧 CPU。
func pushesContextOn(t string) bool {
	switch t {
	case agent.EventUser, agent.EventStep, agent.EventToolResult, agent.EventCompress:
		return true
	}
	return false
}

func (s *Server) contextUsage(sessionID string) map[string]any {
	sess, _ := s.agent.History().Get(sessionID)
	st := s.agent.ContextStat(sess)

	// 千分比取整再除 10 = 一位小数的百分比（避免为此引入 math 依赖）。
	percent := float64(0)
	if st.Budget > 0 {
		percent = float64(st.Used*1000/st.Budget) / 10
	}
	return map[string]any{
		"type":         "context",
		"session_id":   sessionID,
		"used":         st.Used,
		"raw":          st.Raw,
		"budget":       st.Budget,
		"messages":     st.Messages,
		"percent":      percent,
		"compressed":   st.Compressed,
		"over_budget":  st.OverBudget,
		"summarized":   st.Summarized,
		"window":       st.Window,
		"reserve":      st.Reserve,
		"model":        s.cfg.LLM.DisplayName,
		"model_id":     s.cfg.LLM.Model,
		"total_tokens": st.TotalTokens, // 累计消耗（输入+输出，内存态）
		"cache_hit":    st.CacheHit,    // 累计缓存命中 tokens
		"cache_miss":   st.CacheMiss,   // 累计缓存未命中 tokens
	}
}

// todoEvent 构造推送给前端的「任务清单」事件帧。
// 会话存在与否都返回合法帧：无会话/无清单时 todos 为空数组，前端渲染为空态。
func (s *Server) todoEvent(sessionID string) map[string]any {
	return map[string]any{
		"type":       "todo",
		"session_id": sessionID,
		"todos":      s.agent.Todos(sessionID),
	}
}

// historyEvent 构造「会话历史快照」帧。
//
// 与 load_session 下发的是同一种帧（前端统一走 replayHistory 重建视图），
// 区别只在触发时机：load_session 是用户切会话，这里是**历史刚被截断**。
// 会话不存在时返回空消息列表的合法帧，前端渲染成空态即可，不必报错。
//
// 必须走快照：本函数由别的连接（切会话查看）或同连接的收尾阶段调用，
// 与运行中的循环并发；直接序列化 sess.Messages 会边读边写。
func (s *Server) historyEvent(sessionID string) map[string]any {
	sess, ok := s.agent.History().Get(sessionID)
	if !ok {
		return map[string]any{"type": "history", "session_id": sessionID, "messages": []any{}}
	}
	title, messages := sess.SnapshotForRender()
	return map[string]any{
		"type":       "history",
		"session_id": sess.ID,
		"title":      title,
		"messages":   messages,
	}
}

// checkpointEvent 构造推送给前端的「检查点列表」事件帧（回滚菜单的数据源）。
// 列表按步骤聚合（时间 + 文件数），前端据此列出候选回滚点。
func (s *Server) checkpointEvent(sessionID string) map[string]any {
	return map[string]any{
		"type":       "checkpoints",
		"session_id": sessionID,
		"steps":      s.agent.CheckpointSteps(sessionID),
		"editables":  s.agent.EditableUserMessages(sessionID, editableUserMessageLimit),
		// resume_back：最后一轮「没有完整结束」的锚点（back），否则 -1。
		// 前端据此在打断 / 报错 / 刷新页面后挂出**「继续」**按钮 —— 判据在服务端算，
		// 页面刷新后视图是无状态的，前端自己猜不出来。
		//
		// ⚠️ 名字必须是 resume 而不是 retry：这个动作是「追加一句『继续』接着跑」，
		// 不截断历史、不回退文件。破坏性的重来入口只剩「重新生成」与「编辑重发」
		//（2026-09-22 改的语义；字段名到 2026-09-23 才跟上，此前叫 retry_back，
		// 名字与行为不符，长期共存会误导后续维护）。
		"resume_back": s.agent.UnfinishedTurnAnchor(sessionID),
	}
}

// editableUserMessageLimit 是允许编辑的用户消息条数（最近 N 条）。
// 限制范围是有意的：更早的历史可能已被摘要压缩掉，改它会让压缩摘要与实际
// 历史对不上（摘要里还留着旧提问）。
const editableUserMessageLimit = 3

// clipText 按「字符」截断文本（避免把多字节汉字截成半个），超长时追加省略号。
func clipText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// stop 取消当前正在执行的任务，返回是否有任务被取消。
func (c *wsClient) stop() bool {
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()
	if c.cancel == nil {
		return false
	}
	c.cancel()
	c.cancel = nil
	return true
}

// ---------------------------------------------------------------------------
// HITL 审批器（实现 tools.Approver）
// ---------------------------------------------------------------------------

type wsApprover struct {
	client  *wsClient
	mu      sync.Mutex
	next    int
	pending map[string]chan bool
	// sessionID 是当前审批所属的会话：前端可能已切到别的会话查看，
	// 审批卡片要能标注来源，避免被误认为当前会话的操作。
	sessionID string
}

// setSession 记录本轮任务所属会话（在 run 启动时调用）。
func (a *wsApprover) setSession(id string) {
	a.mu.Lock()
	a.sessionID = id
	a.mu.Unlock()
}

// currentSession 返回当前审批所属会话（无则空串）。
func (a *wsApprover) currentSession() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

// RequestApproval 向浏览器推送审批请求并阻塞等待决策。
func (a *wsApprover) RequestApproval(ctx context.Context, req tools.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	a.next++
	id := fmt.Sprintf("ap_%d", a.next)
	ch := make(chan bool, 1)
	a.pending[id] = ch
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	a.client.send(map[string]any{
		"type":        "hitl_request",
		"approval_id": id,
		"session_id":  a.currentSession(),
		"tool":        req.Tool,
		"action":      req.Action,
		"reason":      req.Reason,
		"detail":      req.Detail,
		"diff":        req.Diff,
	})

	select {
	case approved := <-ch:
		return approved, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// resolve 处理前端返回的审批决策。
func (a *wsApprover) resolve(id string, approved bool) {
	a.mu.Lock()
	ch, ok := a.pending[id]
	a.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- approved:
	default:
	}
}
