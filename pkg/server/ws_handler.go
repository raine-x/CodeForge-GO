package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"

	"codeforge/pkg/agent"
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
	Type       string `json:"type"`
	SessionID  string `json:"session_id"`
	Text       string `json:"text"`
	Title      string `json:"title"`
	ApprovalID string `json:"approval_id"`
	Approved   bool   `json:"approved"`
	Thinking   string `json:"thinking"` // 思考强度：low/medium/high
}

// wsClient 表示一个浏览器 WebSocket 连接。
type wsClient struct {
	srv      *Server
	conn     *websocket.Conn
	writeMu  sync.Mutex
	approver *wsApprover

	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

func (c *wsClient) send(v any) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.WriteJSON(v)
}

// handleWS 处理 WebSocket 连接：接收用户消息、推送事件流、处理 HITL 决策。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] 升级失败: %v", err)
		return
	}
	client := &wsClient{srv: s, conn: conn}
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
		if strings.TrimSpace(msg.Text) == "" {
			return
		}
		sessionID := msg.SessionID
		if sessionID == "" {
			sess, err := c.srv.agent.History().Create(ws, "")
			if err != nil {
				c.send(map[string]any{"type": "error", "error": err.Error()})
				return
			}
			sessionID = sess.ID
		}
		go c.run(sessionID, msg.Thinking, func(ctx context.Context, emit func(agent.Event)) error {
			return c.srv.agent.Run(ctx, sessionID, msg.Text, emit)
		})

	case "regenerate":
		// 重新生成：回退会话历史中最后一轮回复，基于同一条用户提问重跑。
		if msg.SessionID == "" {
			return
		}
		go c.run(msg.SessionID, msg.Thinking, func(ctx context.Context, emit func(agent.Event)) error {
			return c.srv.agent.Regenerate(ctx, msg.SessionID, emit)
		})

	case "new_session":
		sess, err := c.srv.agent.History().Create(ws, msg.Title)
		if err != nil {
			c.send(map[string]any{"type": "error", "error": err.Error()})
			return
		}
		c.send(map[string]any{"type": "session", "session_id": sess.ID, "title": sess.Title})
		c.send(map[string]any{"type": "sessions", "items": c.srv.agent.History().List("", false)})

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
		c.send(map[string]any{"type": "history", "session_id": sess.ID, "title": sess.Title, "messages": sess.Messages})

	case "hitl_decision":
		c.approver.resolve(msg.ApprovalID, msg.Approved)

	case "cancel":
		// 打断当前任务：由 run() 统一收尾发送 idle；无任务时兜底复位前端状态。
		if !c.stop() {
			c.send(map[string]any{"type": "idle", "reason": "cancelled"})
		}
	}
}

// run 在独立 goroutine 中执行一轮 Agent 交互（正常对话或重新生成）。
func (c *wsClient) run(sessionID, thinking string, agentFn func(ctx context.Context, emit func(agent.Event)) error) {
	c.stop()

	ctx, cancel := context.WithCancel(context.Background())
	c.cancelMu.Lock()
	c.cancel = cancel
	c.cancelMu.Unlock()
	defer cancel()

	// 将本连接的审批器注入 context，使 HITL 请求路由到当前浏览器。
	ctx = tools.WithApprover(ctx, c.approver)
	// 注入子智能体事件转发槽：SubagentRunner 里的实时进度经此推给前端。
	ctx = tools.WithSubagentSink(ctx, func(sev tools.SubagentEvent) {
		if ctx.Err() != nil {
			return // 主任务已取消：不再转发子智能体事件
		}
		c.send(map[string]any{"type": "subagent", "subagent": sev})
	})
	// 注入思考强度（low/medium/high），由 LLM 适配器转换为厂商参数。
	if thinking != "" {
		ctx = agent.WithThinking(ctx, thinking)
	}

	// 主动打断引发的中间层错误不作为故障下发（前端点击 ▶ 打断属正常操作）。
	emit := func(ev agent.Event) {
		if ev.Type == agent.EventError && ctx.Err() != nil {
			return
		}
		c.send(ev)
	}

	c.send(map[string]any{"type": "session", "session_id": sessionID})
	c.send(map[string]any{"type": "busy"})

	runErr := agentFn(ctx, emit)
	if runErr != nil && ctx.Err() == nil {
		c.send(map[string]any{"type": "error", "error": runErr.Error()})
	}
	c.send(map[string]any{"type": "idle"})
	c.send(map[string]any{"type": "sessions", "items": c.srv.agent.History().List("", false)})

	// 任务结束后发送系统通知。通知失败只记日志，不影响对话结果；主动取消不算完成通知。
	if ctx.Err() == nil {
		title, message := "CodeForge", "任务已完成"
		if runErr != nil {
			title, message = "CodeForge", "任务执行失败："+runErr.Error()
		}
		go func() {
			if err := platform.Notify(title, message); err != nil {
				log.Printf("系统通知发送失败（已忽略）：%v", err)
			}
		}()
	}
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
