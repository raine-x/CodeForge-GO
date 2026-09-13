package agent

import "context"

// thinkingKey 是思考强度在 context 中的键。
type thinkingKey struct{}

// WithThinking 将思考强度（low/medium/high）注入 context。
func WithThinking(ctx context.Context, level string) context.Context {
	return context.WithValue(ctx, thinkingKey{}, level)
}

// ThinkingFromCtx 从 context 取出思考强度（未设置返回空）。
func ThinkingFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(thinkingKey{}).(string)
	return v
}
