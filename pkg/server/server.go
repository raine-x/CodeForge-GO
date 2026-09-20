// Package server 提供 Web HTTP & WebSocket 服务。
//
// 安全设计：启动时生成随机 Token，通过 HttpOnly Cookie 下发给浏览器，
// 前端不接触、也不展示 Token；API 与 WebSocket 均校验 Cookie。
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codeforge/config"
	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
	"codeforge/pkg/platform"
	"codeforge/pkg/tools"
	"codeforge/pkg/tools/plugins"
	"codeforge/web"
)

// TokenCookie 是承载会话令牌的 Cookie 名。
const TokenCookie = "codeforge_token"

// ShutdownHeader 是内部关闭接口（/api/shutdown）的令牌请求头，
// 供 codeforge stop 命令使用，与浏览器 Cookie 通道隔离。
const ShutdownHeader = "X-CodeForge-Token"

// Undoer 抽象文件撤销能力（由 builtin.FS 实现）。
type Undoer interface {
	Undo() (string, bool)
	UndoDepth() int
	Root() string
}

// Server 是 CodeForge 的 Web 服务。
type Server struct {
	cfg      *config.Config
	agent    *agent.Agent
	executor *tools.Executor
	registry *tools.Registry
	fs       Undoer
	token    string
	srv      *http.Server
	listener net.Listener

	// modelStore 是设置页统一管理的模型库（config/models.yaml）。
	modelStore *config.ModelStore
	// providerStore 是模型连接信息的归属地（config/providers.yaml）：
	// 模型条目只留 id，Base URL / 协议 / 密钥由它统一持有。
	providerStore *config.ProviderStore

	// plugins 是插件配置（侧栏 MCP 入口展示用）。
	plugins []config.PluginConfig
	// pluginManager 供 /api/plugins 添加/启停后热加载。
	pluginManager *plugins.Manager
	// builtinApply 内置插件开关的运行时应用钩子（main.go 注入）。
	builtinApply func(id string, on bool)
	// subagentApply 子智能体策略变更后的运行时应用钩子（main.go 注入）。
	subagentApply func()

	// done 在服务开始关闭后关闭，供主流程与信号等待统一收口。
	done       chan struct{}
	shutdownMu sync.Once

	// termux-tools 安装状态（安卓 Termux 平台的建议弹窗用，见 termux.go）。
	termuxState atomic.Int32
	termuxErr   atomic.Value // 最近一次失败的输出尾部（string）
}

// SetPluginManager 注入插件管理器（供 /api/plugins 热加载）。
func (s *Server) SetPluginManager(m *plugins.Manager) { s.pluginManager = m }

// New 构造 Web 服务。
func New(cfg *config.Config, ag *agent.Agent, executor *tools.Executor, registry *tools.Registry, fsys Undoer) *Server {
	s := &Server{
		cfg:           cfg,
		agent:         ag,
		executor:      executor,
		registry:      registry,
		fs:            fsys,
		token:         randomToken(),
		modelStore:    config.NewModelStore(modelStorePath(cfg)),
		providerStore: config.NewProviderStore(providerStorePath(cfg)),
		plugins:       cfg.Plugins,
		done:          make(chan struct{}),
	}
	// 把旧 models.yaml（每条模型各自保存 base_url + 密钥）归并成供应商。
	// 失败只记日志不阻断启动：迁移不了顶多维持旧格式，模型照常可用。
	if err := s.ensureProvidersMigrated(); err != nil {
		log.Printf("[server] 供应商归并失败（模型仍按自带连接信息工作）: %v", err)
	}
	// 启动即把当前生效模型的上下文窗口同步给 Agent：压缩阈值（窗口 × 95%）
	// 从第一轮对话就生效，而不是等用户在设置里点一次「应用」。
	s.SyncContextWindow()
	return s
}

// modelStorePath 返回模型库落盘路径。
//
// 配置目录已知时是 <configDir>/models.yaml；未知（空串，仅测试或异常启动会出现）
// 时返回空串，让模型库退化成纯内存库 —— 否则 filepath.Join("", "models.yaml")
// 会得到相对路径，把 models.yaml 写进进程的当前工作目录。
func modelStorePath(cfg *config.Config) string {
	if dir := cfg.ConfigDir(); dir != "" {
		return filepath.Join(dir, "models.yaml")
	}
	return ""
}

// ModelStore 返回模型库（懒加载；供 /api/models/* 使用）。
func (s *Server) ModelStore() *config.ModelStore {
	return s.modelStore
}

// Done 返回关闭信号 channel：无论是收到信号还是收到 /api/shutdown，
// 服务开始关闭时该 channel 即被关闭。
func (s *Server) Done() <-chan struct{} { return s.done }

// Token 返回本次运行的访问令牌（仅用于日志与内部校验，不对外暴露给前端）。
func (s *Server) Token() string { return s.token }

// Port 返回监听端口（唯一来源：全局配置文件 config/default.yaml 的 server.port）。
func (s *Server) Port() int { return s.cfg.Server.Port }

// Addr 返回监听地址。
func (s *Server) Addr() string {
	return net.JoinHostPort(s.cfg.Server.Host, strconv.Itoa(s.cfg.Server.Port))
}

// URL 返回访问地址（不含任何 Token 参数）。
func (s *Server) URL() string {
	host := s.cfg.Server.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d/", host, s.Port())
}

// Routes 构造 HTTP 路由表（便于测试注入）。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/config", s.requireAuth(s.handleConfig))
	mux.HandleFunc("/api/perm", s.requireAuth(s.handlePerm))
	mux.HandleFunc("/api/notify", s.requireAuth(s.handleNotifyPrefs))
	mux.HandleFunc("/api/tree", s.requireAuth(s.handleTree))
	mux.HandleFunc("/api/sessions", s.requireAuth(s.handleSessions))
	mux.HandleFunc("/api/sessions/rewind", s.requireAuth(s.handleRewind))
	mux.HandleFunc("/api/workspaces", s.requireAuth(s.handleWorkspaces))
	mux.HandleFunc("/api/memory", s.requireAuth(s.handleMemory))
	mux.HandleFunc("/api/skills", s.requireAuth(s.handleSkills))
	mux.HandleFunc("/api/plugins", s.requireAuth(s.handlePlugins))
	mux.HandleFunc("/api/builtin-plugins", s.requireAuth(s.handleBuiltinPlugins))
	mux.HandleFunc("/api/subagents", s.requireAuth(s.handleSubagentPrefs))
	mux.HandleFunc("/api/undo", s.requireAuth(s.handleUndo))
	mux.HandleFunc("/api/workspace", s.requireAuth(s.handleWorkspace))
	mux.HandleFunc("/api/pick_folder", s.requireAuth(s.handlePickFolder))
	mux.HandleFunc("/api/pick_file", s.requireAuth(s.handlePickFile))
	mux.HandleFunc("/api/appearance", s.requireAuth(s.handleAppearance))
	mux.HandleFunc("/api/appearance/background", s.requireAuth(s.handleAppearanceBackground))
	mux.HandleFunc("/api/appearance/pick", s.requireAuth(s.handleAppearancePick))
	mux.HandleFunc("/api/stage_file", s.requireAuth(s.handleStageFile))
	mux.HandleFunc("/api/upload_file", s.requireAuth(s.handleUploadFile))
	mux.HandleFunc("/api/termux/tools", s.requireAuth(s.handleTermuxTools))
	mux.HandleFunc("/api/models/test", s.requireAuth(s.handleModelTest))
	mux.HandleFunc("/api/models/discover", s.requireAuth(s.handleModelDiscover))
	mux.HandleFunc("/api/models/save_batch", s.requireAuth(s.handleModelSaveBatch))
	mux.HandleFunc("/api/models/list", s.requireAuth(s.handleModelList))
	mux.HandleFunc("/api/models/save", s.requireAuth(s.handleModelSave))
	mux.HandleFunc("/api/models/delete", s.requireAuth(s.handleModelDelete))
	mux.HandleFunc("/api/models/apply", s.requireAuth(s.handleModelApply))
	mux.HandleFunc("/api/providers/list", s.requireAuth(s.handleProviderList))
	mux.HandleFunc("/api/providers/save", s.requireAuth(s.handleProviderSave))
	mux.HandleFunc("/api/providers/delete", s.requireAuth(s.handleProviderDelete))
	mux.HandleFunc("/api/shutdown", s.handleShutdown)
	mux.HandleFunc("/ws", s.requireAuth(s.handleWS))
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

// Start 启动 HTTP 服务（非阻塞）。
//
// 端口固定取自全局配置文件（config/default.yaml 的 server.port），
// **不做自动换端口**：若端口被占用则直接返回错误，由使用者显式处理
// （关闭占用进程，或修改配置文件中的端口）。
func (s *Server) Start() error {
	addr := s.Addr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			return fmt.Errorf("监听 %s 失败：端口已被占用。"+
				"请执行 codeforge stop 关闭现有实例（或手动关闭占用进程），"+
				"或修改全局配置 config/default.yaml 中的 server.port（当前 %d）",
				addr, s.cfg.Server.Port)
		}
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}

	s.listener = ln
	s.srv = &http.Server{
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[server] 服务退出: %v", err)
		}
	}()
	return nil
}

// isAddrInUse 判断错误是否为「端口已被占用」。
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	// Windows 的报错文案与 POSIX 不同，补一层文本兜底。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address") ||
		strings.Contains(msg, "address in use")
}

// Shutdown 优雅关闭服务。
func (s *Server) Shutdown(ctx context.Context) error {
	defer s.shutdownMu.Do(func() { close(s.done) })
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// handleShutdown 处理内部关闭请求：校验 ShutdownHeader 令牌后触发优雅关闭。
// 供 codeforge stop / restart 命令调用，不走浏览器 Cookie 通道。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(ShutdownHeader)), []byte(s.token)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"ok":true}`))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()
}

// ---------------------------------------------------------------------------
// 鉴权与静态资源
// ---------------------------------------------------------------------------

// ensureCookie 在静态页响应中总是重发令牌 Cookie：
// 服务重启会生成新 Token，若浏览器持有旧 Cookie 而不覆盖，
// 将陷入永久 401，因此每次加载页面都刷新会话。
func (s *Server) ensureCookie(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     TokenCookie,
		Value:    s.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// validToken 校验请求携带的令牌 Cookie。
func (s *Server) validToken(r *http.Request) bool {
	c, err := r.Cookie(TokenCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1
}

// requireAuth 包装处理器，强制令牌校验。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.validToken(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next(w, r)
	}
}

// handleRoot 提供前端静态资源（SPA 回退到 index.html）。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	s.ensureCookie(w, r)

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	dist := web.Dist()
	data, err := fs.ReadFile(dist, name)
	if err != nil {
		name = "index.html"
		data, err = fs.ReadFile(dist, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}

	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// randomToken 生成 32 位十六进制随机令牌。
func randomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// rebuildProvider 按当前配置重建 LLM 适配器，并同步 Agent 的请求参数
// （MaxTokens/Temperature 随模型条目「输出上限」等热切换）与上下文窗口
// （压缩阈值随之切换）。
func (s *Server) rebuildProvider() error {
	p, err := llm.NewProvider(s.cfg.LLM)
	if err != nil {
		return err
	}
	s.agent.SetProvider(p)
	s.agent.SetLLMConfig(s.cfg.LLM)
	s.SyncContextWindow()
	return nil
}

// SyncContextWindow 把当前生效模型条目里的「输入上下文窗口」（ctx_in）
// 同步给 Agent，作为自动压缩阈值的依据。
//
// 该字段以往只存不用：模型库填了 ctx_in 但压缩阈值仍是写死的
// agent.context_token_budget，导致换小窗口模型时压缩不触发、直接超窗。
// 未配置（0）时传 0，Agent 侧自动回退到 context_token_budget。
func (s *Server) SyncContextWindow() {
	window := 0
	if m, ok := s.ModelStore().Find(s.cfg.LLM.Model); ok {
		window = m.CtxIn
	}
	s.agent.SetContextWindow(window)
}

// platformName 返回平台名（供健康检查使用）。
func platformName() string { return platform.OSName() }
