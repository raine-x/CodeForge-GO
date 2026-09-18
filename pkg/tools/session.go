// session.go 定义工具运行时的会话级上下文（工具由此知道自己在哪个会话/第几步跑），
// 以及配套的 TaskSink（后台任务等异步进度的转发槽）。
package tools

import "context"

// sessionScopeKey 是 context 中「会话运行域」的键类型。
type sessionScopeKey struct{}

// SessionScope 描述一次工具调用所属的会话运行域。
// 由 Agent 主循环在每次工具执行前注入（runLoopWithPersistence），
// 后台任务 / 检查点 / 任务清单等「按会话归属」的工具行为依赖它。
type SessionScope struct {
	SessionID string // 会话 ID
	Step      int    // 主循环第几步（从 1 起）
}

// WithSession 将会话运行域注入 context。
func WithSession(ctx context.Context, sc SessionScope) context.Context {
	return context.WithValue(ctx, sessionScopeKey{}, sc)
}

// SessionFrom 取出会话运行域；不在会话内（如子智能体临时循环）时 ok=false。
func SessionFrom(ctx context.Context) (SessionScope, bool) {
	v, ok := ctx.Value(sessionScopeKey{}).(SessionScope)
	return v, ok
}

// taskSinkKey 是 context 中「后台任务事件转发槽」的键类型。
type taskSinkKey struct{}

// TaskEvent 描述一条后台任务的生命周期事件（推送给前端）。
type TaskEvent struct {
	ID      string `json:"id"`                // 任务 ID
	Status  string `json:"status"`            // started / done / failed / cancelled
	Command string `json:"command,omitempty"` // 触发的命令（日志/兜底判断用）
	Exit    int    `json:"exit,omitempty"`    // 退出码（done 时）
	Error   string `json:"error,omitempty"`   // 失败原因（failed 时）
}

// TaskSink 接收后台任务事件（由 WS 层实现，转发给浏览器）。
type TaskSink func(ev TaskEvent)

// WithTaskSink 将后台任务事件转发槽注入 context；无 sink 时事件被丢弃。
func WithTaskSink(ctx context.Context, sink TaskSink) context.Context {
	return context.WithValue(ctx, taskSinkKey{}, sink)
}

// TaskSinkFrom 取出事件转发槽；不存在时 ok=false。
func TaskSinkFrom(ctx context.Context) (TaskSink, bool) {
	v, ok := ctx.Value(taskSinkKey{}).(TaskSink)
	return v, ok && v != nil
}

// checkpointSinkKey 是 context 中「文件写入快照槽」的键类型。
type checkpointSinkKey struct{}

// CheckpointEvent 描述一次「写工具执行前」的文件快照（用于按步回滚）。
//
// 负载之所以带上 OldContent（而不是只给路径让实现方自己读文件），是因为
// 上报发生在写入**之前**：实现方一旦延后读取，读到的就已经是被覆盖后的内容，
// 快照失去意义。工具层不引入 store 依赖，落库与去重由 Agent 侧完成。
type CheckpointEvent struct {
	Path       string `json:"path"`        // 被写入文件的绝对路径
	Existed    bool   `json:"existed"`     // 写入前是否存在（false ⇒ 回滚时删除该文件）
	OldContent string `json:"old_content"` // 写入前的完整内容（Existed 时有效）
}

// CheckpointSink 接收文件写入前的快照（由 Agent 注入：按 (会话, 步骤, 路径) 去重后落库）。
type CheckpointSink func(ev CheckpointEvent)

// WithCheckpointSink 将文件快照槽注入 context；无 sink 时快照被丢弃（如子智能体临时循环）。
func WithCheckpointSink(ctx context.Context, sink CheckpointSink) context.Context {
	return context.WithValue(ctx, checkpointSinkKey{}, sink)
}

// CheckpointSinkFrom 取出文件快照槽；不存在时 ok=false。
func CheckpointSinkFrom(ctx context.Context) (CheckpointSink, bool) {
	v, ok := ctx.Value(checkpointSinkKey{}).(CheckpointSink)
	return v, ok && v != nil
}

// todoSinkKey 是 context 中「任务清单刷新槽」的键类型。
type todoSinkKey struct{}

// TodoSink 在 todo_write 更新清单后触发（由 WS 层注入：刷新 Agent 缓存 + 推前端事件）。
// 负载只传 sessionID —— 最新清单由实现方自行拉取，避免在 tools 层引入 store 依赖。
type TodoSink func(sessionID string)

// WithTodoSink 将任务清单刷新槽注入 context；无 sink 时刷新被丢弃。
func WithTodoSink(ctx context.Context, sink TodoSink) context.Context {
	return context.WithValue(ctx, todoSinkKey{}, sink)
}

// TodoSinkFrom 取出清单刷新槽；不存在时 ok=false。
func TodoSinkFrom(ctx context.Context) (TodoSink, bool) {
	v, ok := ctx.Value(todoSinkKey{}).(TodoSink)
	return v, ok && v != nil
}
