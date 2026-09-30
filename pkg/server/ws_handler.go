package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"codeforge/pkg/agent"
	"codeforge/pkg/errs"
	"codeforge/pkg/logx"
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

	// runs 是**按会话 id** 的运行表，不是单个 cancel 槽。
	//
	// 曾经只有一个 cancel 槽，于是同一连接上**无法并行跑两个会话**：
	// 在 B 会话发消息会先把 A 那一轮停掉（startUserMessage 里的 c.stop()），
	// 打断按钮也会停掉「最近起的那一轮」而不是「你看的那个」。
	// 两个症状都被用户报成「消息跑进另一个会话」。
	//
	// 现在每个会话各占一个槽：起一轮只影响那个会话，打断也只影响那个会话。
	runsMu sync.Mutex
	runs   map[string]*sessionRun

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

// sessionRun 是单个会话当前那一轮的运行态。
type sessionRun struct {
	cancel context.CancelFunc
	seq    int
	// superseded 表示这一轮已被同会话的新一轮取代。
	//
	// 为什么要单独一个标记，而不是靠「槽还在不在」判断：
	// 打断（stopSession）会**立即删掉槽**，若 owns() 判据是「槽里还有我」，
	// 那么被打断的那轮 owns() 就是 false → 跳过收尾 → 永远不发 idle，
	// 前端的运行态卡在「忙」，按钮一直是打断态。
	//
	// 所以改成显式记录「我是否已被取代」：槽删了不等于我被取代。
	superseded bool
}

// runFor 返回该会话当前那一轮；没有则 nil。
func (c *wsClient) runFor(sessionID string) *sessionRun {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()
	if c.runs == nil {
		return nil
	}
	return c.runs[sessionID]
}

// beginRun 登记一轮运行：把该会话的旧轮**标记为已被取代并取消它**，再登记新的。
//
// 旧轮不 join：它异步收尾，用 owns() 挡掉迟到的 idle / error，
// 不会覆盖新一轮的运行态。但 ctx **必须**在这里被取消 —— 否则旧轮会一直卡在
// Agent.beginRunWait 的让位逻辑里（上限 15 秒），新轮迟迟起不来。
func (c *wsClient) beginRun(sessionID string, cancel context.CancelFunc) {
	c.runsMu.Lock()
	old := c.runs[sessionID]
	if old != nil {
		old.superseded = true
	}
	if c.runs == nil {
		c.runs = map[string]*sessionRun{}
	}
	c.runSeq++
	c.runs[sessionID] = &sessionRun{cancel: cancel, seq: c.runSeq}
	c.runsMu.Unlock()

	// 锁外发 cancel：cancel 本身不该在锁内调（它会同步唤醒等在那里的 goroutine）。
	if old != nil {
		old.cancel()
	}
}

// isCurrent 判断 (sessionID, seq) 这一轮是否**未被同会话的新一轮取代**。
//
// ⚠️ 刻意**不**要求「槽里还有我」：打断会立即删掉运行槽，若把删槽当成
// 「被取代」，被打断的那轮就会跳过收尾、永远不发 idle —— 前端运行态
// 卡在「忙」，按钮一直是打断态。
//
// 所以判据只有一个：有没有更新的轮次顶替了我。槽删了不等于被顶替。
func (c *wsClient) isCurrent(sessionID string, seq int) bool {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()
	r := c.runs[sessionID]
	// 槽还在且是我 → 当前轮。
	// 槽不在了 → 可能是被打断（该收尾），也可能是被新轮顶替（不该收尾）。
	// 区分靠 superseded 标记：被打断时没人把它置 true。
	return r == nil || (r.seq == seq && !r.superseded)
}

// endRunIfCurrent 收尾：只有仍是该会话当前那一轮才清槽。
func (c *wsClient) endRunIfCurrent(sessionID string, seq int) {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()
	if c.runs == nil {
		return
	}
	if r := c.runs[sessionID]; r != nil && r.seq == seq {
		delete(c.runs, sessionID)
	}
}

// stopSession 停掉指定会话当前那一轮，返回是否真的停掉了。
//
// 注意：它删的是「运行槽」，而被打断的那轮**仍然要发收尾帧**
// （owns() 判据是 superseded，不是槽还在不在 —— 见 sessionRun 的注释）。
func (c *wsClient) stopSession(sessionID string) bool {
	c.runsMu.Lock()
	r := c.runs[sessionID]
	delete(c.runs, sessionID)
	c.runsMu.Unlock()
	if r == nil || r.superseded {
		return false
	}
	r.cancel()
	return true
}

// runningSessionIDs 列出本连接上正在跑的会话。
func (c *wsClient) runningSessionIDs() []string {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()
	out := make([]string, 0, len(c.runs))
	for id := range c.runs {
		out = append(out, id)
	}
	return out
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
		logx.Errorf("升级失败: %v", err)
		return
	}
	client := &wsClient{srv: s, conn: conn, pageHidden: true}
	client.approver = &wsApprover{client: client, pending: map[string]chan bool{}}

	defer func() {
		// 断连时停掉**本连接上所有**正在跑的会话。
		// 这里不能只停一个 —— runs 是按会话分的，断连意味着整批都没人接收事件了。
		for _, sid := range client.runningSessionIDs() {
			client.stopSession(sid)
		}
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
		// 打断本会话的旧任务必须**同步**发生在启动新 goroutine 之前（dispatch 串行）：
		// 放进 run() 里再停，两条快速连续的消息会让两个 goroutine 都先跑过
		// 登记（cancel 还是 nil），随后第二个覆盖第一个 ——
		// 第一个循环从此无法打断，且两个循环并发写同一会话。
		//
		// ⚠️ 只停**本会话**：曾经这里是 c.stop()，停掉「本连接上最近起的那一轮」，
		// 不分会话 —— 于是「重新生成 B」会把「正在跑的 A」杀掉，
		// 同一连接上也无法并行跑两个会话。
		c.stopSession(msg.SessionID)
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
		c.stopSession(msg.SessionID) // 只打断**本会话**；别的会话并行跑着的不受影响
		go c.run(msg.SessionID, msg.Thinking, "编辑重发", msg.Text, func(ctx context.Context, emit func(agent.Event)) error {
			rollback := msg.RollbackFiles == nil || *msg.RollbackFiles
			if rollback {
				// 检查点按「步骤」归属，而消息截断按「消息」归属，两者没有直接映射。
				// 参见 agent.RewindAfterEdit：无法定位目标消息时返回 nil（跳过文件回滚，
				// 只截断对话），可定位时按其保守口径回退。
				res, err := c.srv.agent.RewindAfterEdit(msg.SessionID, msg.Back)
				if err != nil {
					logx.Errorf("会话=%s 文件回退失败（已跳过，仅重跑对话）：%v", msg.SessionID, err)
				} else if res != nil && len(res.Paths) > 0 {
					emit(agent.Event{Type: agent.EventRewind, Text: "已回退文件改动", Rewind: res})
				}
			}
			// edit 帧由 RerunFrom 自己在「截断之后、跑循环之前」发出，
			// 这里不能再补：晚了会覆盖刚流出的新回复（见 RerunFrom 注释）。
			_, editErr := c.srv.agent.EditAndResend(ctx, msg.SessionID, msg.Back, msg.Text, emit)
			return editErr
		})

	case "continue_turn":
		// 「继续」：从断点接着跑最后一轮（打断 / 报错 / 刷新后）。
		//
		// 不截断历史、不回退文件，也**不追加任何用户消息** —— 早先的实现是
		// 前端发一句字面量「继续」，那会在历史里留下一条用户从未说过的假提问。
		// 破坏性的重来是「重新生成」/「编辑重发」两个显式入口的职责。
		if msg.SessionID == "" {
			return
		}
		c.stopSession(msg.SessionID) // 只打断**本会话**；别的会话并行跑着的不受影响
		go c.run(msg.SessionID, msg.Thinking, "继续上一轮", "", func(ctx context.Context, emit func(agent.Event)) error {
			return c.srv.agent.ContinueTurn(ctx, msg.SessionID, emit)
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
		c.stopSession(msg.SessionID) // 只打断**本会话**；别的会话并行跑着的不受影响
		go c.run(msg.SessionID, msg.Thinking, "断点重试", "", func(ctx context.Context, emit func(agent.Event)) error {
			rollback := msg.RollbackFiles == nil || *msg.RollbackFiles
			if rollback {
				// 检查点按「步骤」归属，而消息截断按「消息」归属，两者没有直接映射。
				// 参见 agent.RewindAfterEdit：无法定位目标消息时返回 nil（跳过文件回滚，
				// 只截断对话），可定位时按其保守口径回退。
				res, err := c.srv.agent.RewindAfterEdit(msg.SessionID, back)
				if err != nil {
					logx.Errorf("会话=%s 文件回退失败（已跳过，仅重跑对话）：%v", msg.SessionID, err)
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
		// ⚠️ 必须**先**切工作区再回放。不切的话文件工具仍用进程全局的
		// FS.root —— 点开 A 项目的会话却去读 B 项目的文件（用户报告的 bug）。
		//
		// 切不过去时**绝不能发 error 帧**。前端的 case 'error' 会无条件执行
		// `sessionChanging = false` 与 `dropOptimisticBubble()`，而这一帧抢在
		// 随后的 history 帧之前到达 —— 切换状态机被打断，前端的 sessionID
		// 永远不更新。用户以为自己在会话 B，实际视图还停在 A，
		// 于是他发的消息被判成「转向后台任务」，落进 A 里。
		// （这条曾经真的发生过：症状是「B 会话的消息跑进 A 会话」+ 顶部一条
		//   「已把这条指令转向到后台正在运行的任务」。）
		//
		// 正确做法：用独立的 workspace_blocked 帧，只提示、不碰切换状态。
		// history / context / todo / checkpoints 四帧照发 —— 读历史不碰工作区，
		// 让用户能看内容；只是继续发消息会落在当前工作区，界面会明确告知。
		blocked := ""
		if !c.srv.switchSessionWorkspace(sess) && sess.Workspace != "" &&
			c.srv.fs.Root() != sess.Workspace {
			blocked = "该会话属于另一个项目，但当前工作区仍是「" +
				filepath.Base(c.srv.fs.Root()) + "」。可以查看历史，" +
				"但继续发消息会读到当前项目的文件；请先停止正在运行的任务。"
		}
		// 内存缓存换成用户想继续的那条（后续 user_message 直接续聊）
		c.send(c.srv.historyEvent(sess.ID))
		c.send(c.srv.contextUsage(sess.ID))
		c.send(c.srv.todoEvent(sess.ID))       // 切会话：回放该会话的任务清单
		c.send(c.srv.checkpointEvent(sess.ID)) // 回放可编辑白名单（编辑按钮的数据源）
		if blocked != "" {
			c.send(map[string]any{"type": "workspace_blocked", "text": blocked})
		}

	case "hitl_decision":
		c.approver.resolve(msg.ApprovalID, msg.Approved)

	case "context":
		// 前端打开上下文进度条明细时主动拉一次最新占用（会话可能刚被切换）。
		c.send(c.srv.contextUsage(msg.SessionID))

	case "visibility":
		// 前端上报页面可见性：窗口在前台可见时，任务完成不弹系统通知（不打扰）。
		c.setVisibility(msg.Hidden)

	case "cancel":
		// 打断**指定会话**当前那一轮。
		//
		// ⚠️ 曾经这里是 c.stop() —— 停掉「本连接上最近起的那一轮」，不管它是
		// 哪个会话。结果：在 B 会话按打断，停掉的是后台的 A；同一个连接也
		// 因此无法并行跑两个会话。现在按会话分槽停。
		//
		// session_id 为空时的处置：**只在恰好有一个会话在跑时才停它**。
		//   - 恰好一个 → 没有歧义，停掉就是用户的意思（老前端不带 id 时的行为）；
		//   - 多个在跑 → 不猜。宁可这次打断不生效，也不能停掉用户没指的那个。
		//     静默什么都不做会让人以为打断坏了，所以回一个 idle 让前端复位。
		if msg.SessionID == "" {
			ids := c.runningSessionIDs()
			switch len(ids) {
			case 0:
				c.send(map[string]any{"type": "idle", "reason": "cancelled"})
			case 1:
				c.stopSession(ids[0])
			default:
				c.send(map[string]any{
					"type":   "idle",
					"reason": "cancelled",
					"note":   "有多个会话正在运行，请切到要打断的那个会话再按打断",
				})
			}
			return
		}
		if !c.stopSession(msg.SessionID) {
			c.send(map[string]any{
				"type": "idle", "reason": "cancelled", "session_id": msg.SessionID,
			})
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
	// ⚠️ 这里**不再** c.stop()。
	//
	// 曾经是「同步打断旧任务后再起新轮」，理由是「否则两条连发消息会让两轮并存」。
	// 但 c.stop() 停的是「本连接上最近起的那一轮」，**不分会话** ——
	// 于是「在 B 会话发消息」会先把「A 会话正在跑的那轮」杀掉。
	// 这就是「同一项目里两个会话没法并发」的根因。
	//
	// 这里不做打断 —— run() 里的 beginRun 会「标记旧轮被取代 + 取消它的 ctx」，
	// 同一会话的重发天然是「打断旧的、起新的」。
	// 不同会话各占一个运行槽，互不干涉 —— 这才是并发要的。
	//
	// ⚠️ 曾经这里是 c.stop()：停掉「本连接上最近起的那一轮」，不分会话 ——
	// 于是「在 B 会话发消息」会先把「A 会话正在跑的那轮」杀掉，
	// 同一连接上也无法并行跑两个会话。
	go c.run(sessionID, msg.Thinking, "用户消息", msg.Text, func(ctx context.Context, emit func(agent.Event)) error {
		media, err := readAttachmentMedia(ws, msg.Attachments)
		if err != nil {
			return err
		}
		return c.srv.agent.RunWithMedia(ctx, sessionID, msg.Text, media, emit)
	})
}

// run 在独立 goroutine 中执行一轮 Agent 交互（正常对话或重新生成）。
// trigger/label 仅用于日志：明确「这一轮是谁、以什么方式触发的」，
// 便于事后排查「我没操作，怎么跑了一轮」这类问题。
func (c *wsClient) run(sessionID, thinking, trigger, label string, agentFn func(ctx context.Context, emit func(agent.Event)) error) {
	// ⚠️ 这里**不**再 c.stop()。
	//
	// 曾经第一行就是 c.stop()，用来「同步打断旧任务后再起新轮」。但 c.stop() 停的
	// 是「本连接上最近起的那一轮」，不分会话 —— 于是 B 会话起新轮会把 A 会话
	// 正在跑的那轮杀掉。同一连接上因此永远只能跑一个会话。
	//
	// 防重入改成按会话：只清**本会话**的旧槽（不同会话各占一个槽，并行跑）。
	// 清槽不 join —— 旧轮异步收尾，它用 owns() 挡掉迟到的 idle，不会覆盖新一轮。
	ctx, cancel := context.WithCancel(context.Background())
	c.beginRun(sessionID, cancel)
	mySeq := c.runFor(sessionID).seq
	defer cancel()

	logx.Runf("会话=%s 触发=%s 输入=%q", sessionID, trigger, clipText(label, 80))

	// owns 判断这一轮是否仍是「该会话当前那一轮」。
	//
	// ⚠️ 判据是「我没被同会话的新一轮取代」，**不是**「槽里还有我」——
	// 打断会立即删槽，用后者会让被打断的那轮跳过收尾、永远不发 idle，
	// 前端运行态卡在「忙」。
	owns := func() bool { return c.isCurrent(sessionID, mySeq) }
	defer c.endRunIfCurrent(sessionID, mySeq)

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
	// 注册目标模式账本：goal_verify 要把判定记到**本轮这个会话**上。
	// 装错会话的话，注入的「目标未通过」提示段永远为空，那条强约束静默失效 ——
	// 而失效的表现恰恰是「看起来一切正常，就是不验证」。
	if ledger, ok := c.srv.agent.GoalLedgerFor(sessionID); ok {
		ctx = tools.WithGoalLedger(ctx, ledger)
	}
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
		// 统一给事件打上会话号：Agent 层不知道会话是谁（一个 Agent 服务多个会话），
		// 由 WS 层在这里补。前端靠它判断「这帧是不是当前视图的」——
		// 之前 Event 没有这个字段，case 'history' 只能无条件重建视图，
		// 而下面 EventEdit 时主动补的那一帧是非请求触发的，
		// 运行中切会话就会把整屏拽回旧会话（loadSession 允许运行中调用）。
		ev.SessionID = sessionID
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
	c.send(map[string]any{"type": "busy", "session_id": sessionID})

	runErr := agentFn(ctx, emit)
	if !owns() {
		// 已被新一轮取代（用户打断后立刻又发了话）：这一轮的收尾全部作废，
		// 由接管它的那一轮负责发 idle / 刷新占用 / 通知。
		logx.Debugf("会话=%s 本轮已被更新的轮次取代，跳过收尾事件", sessionID)
		return
	}
	if runErr != nil && ctx.Err() == nil {
		// 走 errs.FriendlyOr：上游 429 / 401 会被翻译成「被上游限流…」
		// 「鉴权失败，去检查密钥」，而不是把 {"code":"1305",...} 甩到界面上。
		//
		// 同一个文件里的 sendErr 就是这么做的（ws_handler.go:94），只是只用在
		// 250/263/320 三处非 LLM 错误上 —— LLM 主链路一直漏着。
		//
		// FriendlyOr 对未识别的错误原样返回，所以既有的前端断言不受影响。
		c.send(map[string]any{"type": "error", "error": errs.FriendlyOr("生成回复", runErr)})
	}
	c.send(map[string]any{"type": "idle", "session_id": sessionID})
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
		logx.Infof("跳过系统通知（会话=%s）：%s", sessionID, reason)
		return
	}

	title, message := "CodeForge", "任务已完成"
	if label := c.sessionLabel(sessionID); label != "" {
		message = "任务已完成：" + label
	}
	if runErr != nil {
		title, message = "CodeForge", "任务执行失败："+clipText(runErr.Error(), 120)
	}
	logx.Infof("发送系统通知（会话=%s）：%s", sessionID, message)
	go func() {
		if err := platform.Notify(title, message); err != nil {
			logx.Errorf("系统通知发送失败（已忽略）：%v", err)
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
		// ⚠️ 名字必须是 resume 而不是 retry：这个动作是「从断点接着跑」
		// （Agent.ContinueTurn），不截断历史、不回退文件，也不在历史里追加
		// 「继续」这句假提问。破坏性的重来入口只剩「重新生成」与「编辑重发」
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

// stop 已删除。
//
// 它停掉「本连接上最近起的那一轮」，**不分会话** —— 这正是「在 B 会话发消息
// 却把 A 会话正在跑的任务杀掉」「同一连接无法并行跑两个会话」的根因。
// 现在按会话分开：stopSession(sessionID) 只停那一个会话，
// 运行表是 wsClient.runs（见结构体注释）。

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
		// 子任务身份：并行委派时界面上会同时挂多张审批卡，靠它区分
		// （中文短语与 session_id 都相同，光看那两个分不出来）。
		"subagent_id":   req.SubagentID,
		"subagent_mode": req.SubagentMode,
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
