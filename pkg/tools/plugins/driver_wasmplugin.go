package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"codeforge/config"
	"codeforge/pkg/tools"
)

// wasmDriver 基于 wazero 运行 WASM 沙盒插件（零 CGO）。
//
// 约定的导出 ABI：
//
//	alloc(size i32) -> i32        分配内存并返回偏移
//	manifest() -> i64             返回打包指针（高 32 位=长度，低 32 位=偏移）
//	call(ptr i32, len i32) -> i64 入参为 JSON，返回打包指针
type wasmDriver struct {
	cfg config.PluginConfig

	runtime    wazero.Runtime
	mod        api.Module
	allocFn    api.Function
	manifestFn api.Function
	callFn     api.Function

	mu sync.Mutex
}

// newWASMDriver 加载并实例化 WASM 模块。
func newWASMDriver(cfg config.PluginConfig) (*wasmDriver, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, fmt.Errorf("WASM 插件 %s 缺少 path", cfg.Name)
	}
	wasmBytes, err := os.ReadFile(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("读取 wasm 文件失败: %w", err)
	}

	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	mod, err := rt.Instantiate(ctx, wasmBytes)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("实例化 wasm 失败: %w", err)
	}

	d := &wasmDriver{cfg: cfg, runtime: rt, mod: mod}
	d.allocFn = mod.ExportedFunction("alloc")
	d.manifestFn = mod.ExportedFunction("manifest")
	d.callFn = mod.ExportedFunction("call")

	if d.allocFn == nil || d.callFn == nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("WASM 插件缺少必需导出函数 alloc / call")
	}
	return d, nil
}

// Tools 实现 Driver：优先读取 manifest 自描述，否则暴露单工具。
func (d *wasmDriver) Tools(ctx context.Context) ([]tools.Tool, error) {
	fallback := []tools.Tool{d.singleTool()}
	if d.manifestFn == nil {
		return fallback, nil
	}

	results, err := d.manifestFn.Call(ctx)
	if err != nil || len(results) == 0 {
		return fallback, nil
	}
	data, err := d.readPacked(results[0])
	if err != nil || len(data) == 0 {
		return fallback, nil
	}

	var manifest struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Tools) == 0 {
		return fallback, nil
	}

	out := make([]tools.Tool, 0, len(manifest.Tools))
	for _, t := range manifest.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = genericSchema()
		}
		out = append(out, &RemoteTool{
			name:     t.Name,
			desc:     t.Description,
			schema:   schema,
			invokeFn: d.invoke,
		})
	}
	return out, nil
}

func (d *wasmDriver) singleTool() *RemoteTool {
	desc := d.cfg.Description
	if strings.TrimSpace(desc) == "" {
		desc = "WASM 沙盒插件：" + d.cfg.Name
	}
	return &RemoteTool{
		name:     d.cfg.Name,
		desc:     desc,
		schema:   genericSchema(),
		invokeFn: d.invoke,
	}
}

// invoke 调用 WASM 插件的 call 导出函数。
func (d *wasmDriver) invoke(ctx context.Context, name string, args json.RawMessage) (*tools.ToolResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	req, err := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(args)})
	if err != nil {
		return nil, err
	}
	ptr, err := d.writeMemory(ctx, req)
	if err != nil {
		return nil, err
	}

	results, err := d.callFn.Call(ctx, uint64(ptr), uint64(len(req)))
	if err != nil {
		return nil, fmt.Errorf("wasm call 执行失败: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("wasm call 无返回值")
	}
	out, err := d.readPacked(results[0])
	if err != nil {
		return nil, err
	}

	var res struct {
		Success bool   `json:"success"`
		Data    any    `json:"data"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return tools.Ok(string(out)), nil
	}
	result := &tools.ToolResult{Success: res.Success, Data: res.Data}
	if !res.Success {
		result.Error = res.Error
	}
	return result, nil
}

// writeMemory 通过 alloc 分配内存并写入数据。
func (d *wasmDriver) writeMemory(ctx context.Context, data []byte) (uint32, error) {
	results, err := d.allocFn.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("wasm alloc 失败: %w", err)
	}
	if len(results) == 0 {
		return 0, fmt.Errorf("wasm alloc 无返回值")
	}
	ptr := uint32(results[0])

	mem := d.mod.Memory()
	if mem == nil {
		return 0, fmt.Errorf("wasm 模块未导出内存")
	}
	if !mem.Write(ptr, data) {
		return 0, fmt.Errorf("写入 wasm 内存失败 (ptr=%d len=%d)", ptr, len(data))
	}
	return ptr, nil
}

// readPacked 解包 i64 返回值并读取内存内容。
func (d *wasmDriver) readPacked(packed uint64) ([]byte, error) {
	offset := uint32(packed & 0xFFFFFFFF)
	length := uint32(packed >> 32)

	mem := d.mod.Memory()
	if mem == nil {
		return nil, fmt.Errorf("wasm 模块未导出内存")
	}
	data, ok := mem.Read(offset, length)
	if !ok {
		return nil, fmt.Errorf("读取 wasm 内存失败 (offset=%d len=%d)", offset, length)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// Close 实现 Driver：关闭运行时。
func (d *wasmDriver) Close() error {
	if d.runtime == nil {
		return nil
	}
	return d.runtime.Close(context.Background())
}
