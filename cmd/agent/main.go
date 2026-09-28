// Command agent 是 CodeForge-Go 的主程序入口。
//
// 用法：
//
//	codeforge [start]  启动服务（默认行为，前台运行）
//	codeforge stop     停止运行中的服务
//	codeforge restart  重启服务（先停止旧实例，再前台启动新实例）
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codeforge/config"
	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
	"codeforge/pkg/logx"
	"codeforge/pkg/platform"
	"codeforge/pkg/security"
	"codeforge/pkg/server"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
	"codeforge/pkg/tools/builtin"
	"codeforge/pkg/tools/plugins"
)

func main() {
	// 尽早设内存软上限：必须早于任何大分配（工具注册表、SQLite 连接都在后面）。
	// 之前完全没有这个边界，走 Go 默认 GOGC=100 —— 堆翻倍才 GC，
	// 失控时会被 OOMKill 连带丢掉未落盘的会话状态与正在写的审计日志。
	applyMemoryLimit()

	// 子命令：start（默认）/ stop / restart；剥离后其余 flag 照常解析。
	sub := ""
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start", "stop", "restart":
			sub = os.Args[1]
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `CodeForge - 跨平台 Web Agent

用法:
  codeforge [start]  [-config 目录] [-workdir 目录] [-no-open]   启动服务（默认）
  codeforge stop     [-config 目录]                              停止运行中的服务
  codeforge restart  [-config 目录] [-workdir 目录] [-no-open]   重启服务

会话续跑（回到上次中断的地方继续）:
  codeforge -continue                 打开上次那个会话
  codeforge -resume last              同上
  codeforge -resume <会话ID前缀>       打开指定会话（ID / ID 前缀 / 标题均可）
  codeforge -continue -workdir <目录>  先切到该工作区，再回到它上次的会话
  与已在运行的实例共存时：仅把浏览器指到目标会话（不重启服务）。

选项:
`)
		flag.PrintDefaults()
	}
	configDir := flag.String("config", "config", "配置目录")
	workDir := flag.String("workdir", "", "Agent 工作目录（默认当前目录）")
	noOpen := flag.Bool("no-open", false, "不自动打开浏览器")
	continueLast := flag.Bool("continue", false, "启动后回到上次会话（该工作区最近更新的一条）")
	resumeID := flag.String("resume", "", "启动后回到指定会话：会话 ID / ID 前缀 / 会话标题，或 last")
	flag.Parse()

	resumeTarget := strings.TrimSpace(*resumeID)
	if *continueLast && resumeTarget == "" {
		resumeTarget = "last" // -continue 与 -resume last 是同一件事
	}

	// 配置目录统一解析（含「可执行文件旁」回退），stop / restart / start 共用同一份，
	// 否则在错误 CWD 下 `stop` 会找不到运行信息文件。
	*configDir = resolveConfigDir(*configDir)

	switch sub {
	case "stop":
		stopInstanceCmd(*configDir)
		return
	case "restart":
		stopInstanceCmd(*configDir) // 尽力停止旧实例，未运行则直接启动
	}

	os.Exit(startCmd(*configDir, *workDir, *noOpen, resumeTarget))
}

// resumeTarget 是一次会话续跑的定位结果。
type resumeTarget struct {
	SessionID string // 要打开的会话 ID
	Workspace string // 该会话归属的工作区（空 = 未选择工作区的会话）
	Title     string
}

// resolveResumeTarget 把 -continue / -resume 的目标翻译成具体会话。
//
// 匹配顺序：last（该工作区最近更新的一条）→ 会话 ID（精确或唯一前缀）→
// 会话标题唯一。刻意不做「模糊命中一个算一个」：续跑开错会话比开不起来更糟，
// 宁可列出候选让人自己选。
func resolveResumeTarget(h *agent.History, workspace, target string) (*resumeTarget, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, nil
	}
	if target == "last" {
		id := h.Latest(workspace)
		if id == "" {
			if workspace == "" {
				return nil, fmt.Errorf("尚未选择工作区，且该状态下没有会话可续跑")
			}
			return nil, fmt.Errorf("工作区 %s 下还没有会话可续跑", workspace)
		}
		s, ok := h.Get(id)
		if !ok {
			return nil, fmt.Errorf("会话 %s 已不存在", id)
		}
		return &resumeTarget{SessionID: s.ID, Workspace: s.Workspace, Title: s.Title}, nil
	}

	// 跨工作区查找：-resume 允许找回别的项目里的会话，找到后由调用方切工作区。
	var idHits, titleHits []agent.SessionMeta
	all := h.List("", false)
	for _, m := range all {
		if strings.HasPrefix(m.ID, target) {
			idHits = append(idHits, m)
		}
		if strings.EqualFold(strings.TrimSpace(m.Title), target) {
			titleHits = append(titleHits, m)
		}
	}
	pick := func(hits []agent.SessionMeta, what string) (*resumeTarget, error) {
		switch len(hits) {
		case 0:
			return nil, nil
		case 1:
			return &resumeTarget{SessionID: hits[0].ID, Workspace: hits[0].Workspace, Title: hits[0].Title}, nil
		}
		out := make([]string, 0, 6)
		for i, m := range hits {
			if i == 5 {
				out = append(out, "…")
				break
			}
			out = append(out, m.ID)
		}
		return nil, fmt.Errorf("%s %q 匹配到 %d 条会话（%s），请把 -resume 的参数写得更具体",
			what, target, len(hits), strings.Join(out, ", "))
	}
	if got, err := pick(idHits, "会话 ID"); err != nil || got != nil {
		return got, err
	}
	if got, err := pick(titleHits, "会话标题"); err != nil || got != nil {
		return got, err
	}
	return nil, fmt.Errorf("找不到会话 %q（可续跑的会话共 %d 条；-resume last 回到上次那个）", target, len(all))
}

// lookupResume 只读打开用户级会话库，定位续跑目标。
//
// 单独开一次库是因为「实例已在运行」那条路径也要能查 —— 那时不会构造 Agent，
// 而会话库本就是用户级、跨工作区共享的单个 SQLite 文件（WAL，允许多进程读）。
func lookupResume(workspace, target string) (*resumeTarget, error) {
	if strings.TrimSpace(target) == "" {
		return nil, nil
	}
	st, err := store.Open(store.DefaultPath())
	if err != nil {
		return nil, fmt.Errorf("打开会话库失败：%w", err)
	}
	defer st.Close()
	return resolveResumeTarget(agent.NewHistory(st), workspace, target)
}

// applyResumeWorkspace 把续跑会话的工作区落实为本次的运行工作区，返回最终路径。
//
// 会话在哪个项目里谈的，就该在哪个项目里接着谈：否则模型读的是 A 项目的文件，
// 讨论的却是 B 项目的对话。目标目录已经不存在时保持原工作区不变（会话仍可读，
// 只是改不了文件），这比默默切到一个不存在的目录好。
func applyResumeWorkspace(current string, resume *resumeTarget) string {
	if resume.Workspace == current {
		return current
	}
	switch {
	case resume.Workspace == "":
		logx.Infof("会话续跑：会话 %s 创建时尚未选择工作区，本次同样按「未选择工作区」打开", resume.SessionID)
		return ""
	default:
		if st, err := os.Stat(resume.Workspace); err != nil || !st.IsDir() {
			logx.Infof("会话续跑：会话 %s 的工作区 %s 已不存在，仍按当前工作区打开（只读对话，别改文件）",
				resume.SessionID, resume.Workspace)
			return current
		}
		logx.Infof("会话续跑：切到该会话所属工作区 %s（原 %s）", resume.Workspace, displayWorkDir(current))
		return resume.Workspace
	}
}

// withResumeParam 把续跑会话编进浏览器地址：前端首帧后按 ?s= 直接载入那条会话。
func withResumeParam(base string, resume *resumeTarget) string {
	if resume == nil {
		return base
	}
	return base + "/?s=" + url.QueryEscape(resume.SessionID)
}

func openBrowserAt(target string) {
	if err := platform.OpenBrowser(target); err != nil {
		logx.Warnf("自动打开浏览器失败：%v", err)
	}
}

// stopInstanceCmd 停止运行中的实例并输出结果。返回是否实际停止了进程。
func stopInstanceCmd(configDir string) bool {
	port := 0
	if cfg, err := config.Load(configDir); err == nil {
		port = cfg.Server.Port
	}
	if stopInstance(configDir, port) {
		fmt.Println("CodeForge 已停止")
		return true
	}
	fmt.Println("CodeForge 未在运行")
	return false
}

// startCmd 启动服务并阻塞至收到退出信号或内部关闭请求。返回进程退出码。
//
// resumeArg 是 -continue / -resume 的原始参数（空 = 不续跑）：
// 定位到具体会话后，把浏览器直接停在那条会话上
// （会话属于别的工作区时，连工作区一起切过去，避免在一个项目里回放另一个项目的对话）。
func startCmd(configDir, workDir string, noOpen bool, resumeArg string) int {
	// 配置目录体检：缺少 default.yaml 时给出醒目告警（最常见原因是 CWD 不对）。
	warnConfigDir(configDir)

	// 加载 .env（若存在）：仅填充尚未设置的环境变量，不覆盖真实环境变量。
	//
	// 查找顺序：CWD/.env → <configDir>/.env → <configDir>/../.env。
	// 最后一项对应仓库约定（<root>/.env 与 <root>/config 同级），
	// 使得从别处用 -config 指向该配置目录时也能取到项目根目录的 .env。
	if path, err := config.LoadDotEnv(
		".env",
		filepath.Join(configDir, ".env"),
		filepath.Join(configDir, "..", ".env"),
	); err != nil {
		logx.Warnf("读取 .env 失败: %v", err)
	} else if path != "" {
		logx.Infof("已加载环境变量文件：%s", path)
	}

	// 本地覆盖配置：缺失时自动生成带注释的模板（该文件被 .gitignore 忽略）。
	if path, err := config.EnsureLocalTemplate(configDir); err != nil {
		logx.Warnf("生成本地配置模板失败: %v", err)
	} else if path != "" {
		logx.Infof("本地覆盖配置：%s", path)
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		logx.Fatalf("加载配置失败: %v", err)
	}
	if workDir != "" {
		cfg.Agent.WorkDir = workDir
	}

	// 工作目录：来自配置文件（agent.work_dir）或 -workdir；为空表示「未选择工作区」，
	// 由用户在界面显式选择后才挂载（不再兜底到进程启动目录）。
	// 选择会持久化回 local.yaml，重启自动恢复；目录被删时打告警并按未选择处理。
	wd := cfg.Agent.WorkDir
	if abs, err := filepath.Abs(wd); err == nil && wd != "" {
		wd = abs
	}
	if wd != "" {
		if st, err := os.Stat(wd); err != nil || !st.IsDir() {
			logx.Warnf("配置的工作目录 %s 不存在或不是目录，按「未选择工作区」处理（请在界面重新选择）", wd)
			wd = ""
		}
	}

	// 续跑目标先定位：会话可能属于另一个项目，工作区要跟着它走，
	// 所以这一步必须排在 FS / Agent 构造之前。
	// 定位失败（ID 打错、会话已删）只降级为正常启动 —— 一个参数笔误
	// 不该让整个服务起不来。
	var resume *resumeTarget
	if r, err := lookupResume(wd, resumeArg); err != nil {
		logx.Warnf("会话续跑：%v（按正常启动处理）", err)
	} else if resume = r; resume != nil {
		wd = applyResumeWorkspace(wd, resume)
	}

	// 已有实例在运行时拒绝重复启动（以运行信息文件 + 健康检查为准）。
	if info := readRunInfo(configDir); info != nil {
		if healthOK(info.Port) {
			logx.Warnf("CodeForge 已在运行（PID %d）：%s；如需重启请执行 codeforge restart", info.PID, info.URL)
			if resume != nil {
				// 续跑不必重启服务：把浏览器指到那条会话即可。
				// 工作区由界面按会话归属自行切换（旧实例的工作区不改写）。
				logx.Infof("会话续跑：在已运行的实例上打开会话 %s", resume.SessionID)
				openBrowserAt(withResumeParam(info.URL, resume))
			}
			return 0
		}
		removeRunInfo(configDir) // 过期的运行信息，清理后正常启动
	}

	// 1) 工具注册中心与内置工具
	registry := tools.NewRegistry()
	fsys := builtin.NewFS(wd)
	// 工作区约束：默认拒绝访问 agent.work_dir 之外的路径（详见 security.allow_outside_workspace）。
	fsys.SetAllowOutside(cfg.Security.AllowOutsideWorkspace)
	if cfg.Security.AllowOutsideWorkspace {
		logx.Warnf("安全：已允许访问工作区之外的路径（security.allow_outside_workspace=true）")
	}
	builtin.RegisterFS(registry, fsys)
	builtin.RegisterTerminal(registry, fsys)
	builtin.RegisterSearch(registry, fsys)

	// 2) 安全层（策略 + 审计）
	policy := security.NewPolicy(cfg.Security)
	audit, err := security.NewAuditLogger(resolvePath(cfg.AuditLog, wd))
	if err != nil {
		logx.Fatalf("初始化审计日志失败: %v", err)
	}
	defer audit.Close()

	executor := tools.NewExecutor(registry, policy, audit, nil, 120*time.Second, 32*1024)

	// 3) 插件引擎
	manager := plugins.NewManager(registry, policy, cfg.Plugins)
	if err := manager.LoadAll(context.Background()); err != nil {
		logx.Warnf("插件加载告警: %v", err)
	}
	defer manager.Stop()

	// 4) LLM 适配器
	//
	// state.yaml 的 llm 段**只存 model id**（明文密钥不再落盘，见 config.StateLLM），
	// 所以这里必须先按 id 把协议 / base_url / 密钥解析出来，否则 NewProvider
	// 拿到的是一份「只有模型名」的残缺配置。
	//
	// 解析发生在建适配器**之前**：解析不出密钥时还会退回 .env / local.yaml 的值，
	// 而那份配置已经被 Load() 合进来了。
	credentialStores(cfg, configDir)
	provider, err := llm.NewProvider(cfg.LLM)
	if err != nil {
		logx.Fatalf("初始化 LLM 适配器失败: %v", err)
	}

	// 5) Agent 引擎与会话历史（SQLite 用户级库，跨工作区共享、按工作区隔离）
	st, err := store.Open(store.DefaultPath())
	if err != nil {
		logx.Fatalf("初始化 SQLite 存储失败: %v", err)
	}
	defer st.Close()
	// 旧版 JSON 会话一次性迁移（幂等；原文件改名 .imported 保留）
	if n := st.MigrateSessions(resolvePath(cfg.DataDir, wd)); n > 0 {
		logx.Infof("已迁移 %d 条旧版 JSON 会话到 SQLite", n)
	}
	history := agent.NewHistory(st)
	ag := agent.New(cfg.Agent, cfg.LLM, provider, executor, history, wd)
	ag.SetMemoryStore(st)                // 用户记忆与 History 共用同一 SQLite 库
	builtin.RegisterMemory(registry, ag) // save_memory 工具
	// 任务清单工具（存储在 session_todos 表，落库由 Agent 桥接）
	builtin.RegisterTodo(registry, ag)
	// 联网工具（web_fetch / web_search）：web.enabled=false 时自动跳过
	builtin.RegisterWeb(registry, cfg.Web)

	// Exposure 层：按 agent.hidden_tools 隐藏工具（不把定义发给 LLM）。
	// 与「注册期裁剪」（如 web.enabled=false 不注册）互补：hidden_tools 保留注册与
	// 审计链路，只削减 LLM 的可见面；两者都不改变 Executor 与权限判定。
	// 注意：隐藏只影响可见面，不代表禁止执行 —— 执行放行由 Executor/Policy 判定。
	ag.SetExposure(hiddenToolsExposure(cfg.Agent.HiddenTools))

	// 内置插件：Skill Creator（配置 builtin_plugins.skill_creator，缺省开）
	if cfg.BuiltinPlugins.SkillCreatorEnabled() {
		builtin.RegisterSkillCreator(registry, fsys)
		ag.SetSkillCreatorEnabled(true)
		logx.Infof("内置插件已启用：Skill Creator（config: builtin_plugins.skill_creator=false 可关闭）")
	}

	// 内置插件：Multi-Agent（并发与能力限制见 config.subagents）
	//
	// 策略有三个落点，每次设置变更都要一起同步，否则会出现「界面显示 2、
	// 实际放行 5」这种不一致：
	//   1) Agent —— 子智能体工具白名单与任务提示词（newSubagent / subagentPrompt）
	//   2) 调度器校验 —— 一次委派的数量上限（读 Agent 的策略，无需单独同步）
	//   3) 委派工具 —— Description 与 InputSchema 里的 maxItems
	subagentRunner := agent.NewSubagentRunner(ag)
	var subagentTool *builtin.SubagentsTool

	applySubagentPolicy := func() {
		policy := agent.NewSubagentPolicy(*cfg)
		ag.SetSubagentPolicy(policy)
		if subagentTool != nil {
			subagentTool.SetMaxConcurrent(policy.MaxConcurrent)
		}
	}
	applySubagentPolicy() // 主智能体侧先行生效（工具尚未注册时只影响后续创建的子智能体）

	if cfg.BuiltinPlugins.MultiAgentEnabled() {
		subagentTool = builtin.RegisterSubagents(registry, subagentRunner)
		applySubagentPolicy() // 把并发上限同步进工具描述与 schema
		ag.SetMultiAgentEnabled(true)
		logx.Infof("内置插件已启用：Multi-Agent（并发上限 %d，能力：写=%v 删=%v 记忆=%v；config: builtin_plugins.multi_agent=false 可关闭）",
			cfg.SubagentMaxConcurrent(), cfg.SubagentAllowWrite(), cfg.SubagentAllowDelete(), cfg.SubagentAllowMemory())
	}

	// 内置插件：Plan 计划模式（纯 System Prompt 注入，无需注册工具）
	if cfg.BuiltinPlugins.PlanEnabled() {
		ag.SetPlanEnabled(true)
		logx.Infof("内置插件已启用：Plan 计划模式（输入 @plan 出计划书；config: builtin_plugins.plan=false 可关闭）")
	}

	// 内置插件：目标模式（@goal_mode 触发，自主验证目标是否达成）。
	//
	// 审查者与工具必须一起注册：工具执行时要能从 ctx 取到账本（服务端装配），
	// 缺任一方都会得到「内部错误」而不是可用功能。
	goalVerifier := agent.NewGoalVerifier(ag)
	var goalTool *builtin.GoalVerifyTool
	applyGoalRounds := func() {
		if goalTool != nil {
			goalTool.SetMaxRounds(cfg.BuiltinPlugins.GoalModeRounds())
		}
	}
	if cfg.BuiltinPlugins.GoalModeEnabled() {
		goalTool = builtin.RegisterGoalVerify(registry, goalVerifier)
		applyGoalRounds()
		ag.SetGoalModeEnabled(true)
		logx.Infof("内置插件：目标模式已启用（@goal_mode 触发，自循环最多 %d 轮；关闭：config: builtin_plugins.goal_mode=false）",
			cfg.BuiltinPlugins.GoalModeRounds())
	}

	// 归档自动清理：启动即清一次 + 每天定时（归档满 10 天即删）
	purgeArchived := func() {
		if n, err := st.DeleteArchivedOlderThan(10); err != nil {
			logx.Warnf("清理归档会话失败: %v", err)
		} else if n > 0 {
			logx.Infof("已自动清理 %d 条归档满 10 天的会话", n)
		}
	}
	purgeArchived()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			purgeArchived()
		}
	}()

	// 6) Web 服务（端口固定取自全局配置文件，端口被占用时直接失败）
	srv := server.New(cfg, ag, executor, registry, fsys)
	srv.SetPluginManager(manager)             // 供 /api/plugins 添加/启停后热加载
	srv.SetSubagentApply(applySubagentPolicy) // 设置 → 子智能体：并发/能力变更即时生效
	srv.SetBuiltinPluginApply(func(id string, on bool) {
		// 内置插件开关的运行时应用：注册/注销工具 + 同步提示词注入
		if id == agent.BuiltinSkillCreator.ID {
			if on {
				if _, ok := registry.Get("create_skill"); !ok {
					builtin.RegisterSkillCreator(registry, fsys)
				}
			} else {
				registry.Unregister("create_skill")
			}
			ag.SetSkillCreatorEnabled(on)
		}
		if id == agent.BuiltinMultiAgent.ID {
			if on {
				if _, ok := registry.Get("delegate_subagents"); !ok {
					subagentTool = builtin.RegisterSubagents(registry, subagentRunner)
					applySubagentPolicy() // 重新注册的工具要带上当前并发上限
				}
			} else {
				registry.Unregister("delegate_subagents")
				subagentTool = nil
			}
			ag.SetMultiAgentEnabled(on)
		}
		if id == agent.BuiltinPlan.ID {
			// Plan 计划模式无专属工具：只同步 System Prompt 注入开关
			ag.SetPlanEnabled(on)
		}
		if id == agent.BuiltinGoalMode.ID {
			// 目标模式：注册/注销 goal_verify 工具 + 同步提示词开关。
			// ⚠️ 工具与审查者是一对：工具执行时要能从 ctx 取到账本，
			// 两者必须同进同出，否则会出现「开关显示开着、调用却报内部错误」。
			if on {
				if _, ok := registry.Get("goal_verify"); !ok {
					goalTool = builtin.RegisterGoalVerify(registry, goalVerifier)
					applyGoalRounds() // 注册时就得把轮数同步进 schema/描述
				}
			} else {
				registry.Unregister("goal_verify")
				goalTool = nil
			}
			ag.SetGoalModeEnabled(on)
		}
	})
	if err := srv.Start(); err != nil {
		logx.Fatalf("%v", err)
	}
	logx.Startupf("CodeForge 已启动：%s", srv.URL())
	logx.Startupf("监听 %s（端口取自全局配置 %s）", srv.Addr(), filepath.Join(configDir, "default.yaml"))
	logx.Startupf("平台：%s｜工作目录：%s｜可用工具：%d 个", platform.OSName(), displayWorkDir(wd), len(registry.Names()))
	logx.Startupf("模型：%s（provider=%s，API Key %s）", cfg.LLM.Model, cfg.LLM.Provider, keyState(cfg.LLM.APIKey))

	// 记录运行信息，供 codeforge stop / restart 使用（退出时删除）。
	if err := writeRunInfo(&runInfo{
		PID:       os.Getpid(),
		Port:      srv.Port(),
		Token:     srv.Token(),
		URL:       srv.URL(),
		StartedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		logx.Warnf("写入运行信息失败: %v", err)
	}

	// 7) 自动打开浏览器（Token 通过 HttpOnly Cookie 下发，不出现在地址栏）
	if cfg.Server.AutoOpen && !noOpen {
		target := withResumeParam(srv.URL(), resume)
		if resume != nil {
			note := ""
			if t := strings.TrimSpace(resume.Title); t != "" {
				note = "（" + t + "）"
			}
			logx.Infof("会话续跑：打开会话 %s%s", resume.SessionID, note)
		}
		openBrowserAt(target)
	}

	// 8) 等待退出信号或内部关闭请求（codeforge stop），统一优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case <-srv.Done():
	}

	logx.Infof("正在关闭 CodeForge…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	removeRunInfo(configDir)
	return 0
}

// resolveConfigDir 解析配置目录，必要时回退到「可执行文件旁」的同名目录。
//
// 背景：-config 是相对**当前工作目录**解析的，而用户很容易直接双击或运行
// bin/ 下的可执行文件 —— 此时默认值 "config" 解析到 bin/config（不存在），
// provider / 模型 / 外观 / 密钥 全部落回内置默认值，表现成
// 「我上次设的背景图、模型配置，下次启动就没了」，而程序本身照常启动，极难自查。
//
// 回退规则（保守，绝不猜）：
//   - CWD 相对路径下确实有 default.yaml → 原样使用，行为完全不变；
//   - 否则若是相对路径，试 <exe目录>/../<dir>（对应仓库约定 <root>/bin/exe + <root>/config）；
//     那里有 default.yaml 就用它，并打印提示；
//   - 都不满足 → 原样返回，交给 warnConfigDir 告警。
func resolveConfigDir(dir string) string {
	resolved := resolveConfigDirAt(dir, exeDir())
	if resolved != dir {
		abs, _ := filepath.Abs(resolved)
		logx.Warnf("当前工作目录下没有 %s，已自动改用可执行文件旁的配置目录：%s", dir, abs)
		logx.Warnf("直接运行 bin/ 下的可执行文件会导致工作目录不对，建议用项目根目录的启动脚本（cf）。")
	}
	return resolved
}

// exeDir 返回可执行文件所在目录；取不到时返回空串（调用方按「不回退」处理）。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// resolveConfigDirAt 是 resolveConfigDir 的可测版本：exeDir 由调用方注入，
// 避免测试里依赖 os.Executable()（那会指向测试二进制）。
func resolveConfigDirAt(dir, exeDir string) string {
	if _, err := os.Stat(filepath.Join(dir, "default.yaml")); err == nil {
		return dir
	}
	if filepath.IsAbs(dir) || exeDir == "" {
		return dir // 绝对路径还找不到就是真找不到，不猜
	}
	cand := filepath.Join(exeDir, "..", dir)
	if _, err := os.Stat(filepath.Join(cand, "default.yaml")); err != nil {
		return dir
	}
	return cand
}

// warnConfigDir 在配置目录缺少 default.yaml 时给出醒目告警。
//
// 典型误用：在 bin/ 目录下直接执行 ./codeforge.exe。此时 -config 的默认值
// "config" 会相对 CWD 解析到 bin/config/，于是 provider/model 全部落回内置
// 默认值、项目根目录的 .env 也找不到，但程序仍能正常启动，问题极难察觉。
// 这里只告警不中止：内置默认配置本身是合法用法（例如首次体验）。
//
// 注意：resolveConfigDir 已经尽力做过一次「可执行文件旁」的回退，
// 走到这里说明两处都没有 —— 这时告警才是真正需要用户干预的信号。
func warnConfigDir(configDir string) {
	if _, err := os.Stat(filepath.Join(configDir, "default.yaml")); err == nil {
		return
	}
	abs, _ := filepath.Abs(configDir)
	exe, _ := os.Executable()
	logx.Warnf("配置目录 %s 下没有 default.yaml，将使用内置默认配置。", abs)
	logx.Warnf("-config 是相对当前工作目录解析的，这通常说明 CWD 不对。")
	logx.Warnf("请在项目根目录下重试，例如：cd <项目根目录> && %s restart -config config", exe)
}

// keyState 返回 API Key 的加载状态（只输出状态，绝不输出密钥内容）。
func keyState(key string) string {
	if strings.TrimSpace(key) == "" {
		return "未配置"
	}
	return "已加载"
}

// resolvePath 将相对路径解析到工作目录下。
func resolvePath(p, base string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// displayWorkDir 供日志展示：空工作目录显示为「未选择」。
func displayWorkDir(wd string) string {
	if strings.TrimSpace(wd) == "" {
		return "未选择（等待用户在界面选择工作区）"
	}
	return wd
}

// hiddenToolsExposure 把 agent.hidden_tools 配置转成 Exposure 过滤函数：
// 集合中的工具不暴露给 LLM，其余全量。空列表返回 nil（暴露全部）。
func hiddenToolsExposure(hidden []string) func(name string) bool {
	if len(hidden) == 0 {
		return nil
	}
	blocked := make(map[string]bool, len(hidden))
	for _, h := range hidden {
		blocked[h] = true
	}
	return func(name string) bool { return !blocked[name] }
}

// credentialStores 按 cfg.LLM.Model 把凭据解析进 cfg.LLM。
//
// 单独抽成函数是为了让它可测：启动流程本身很难在测试里跑，而「只存 model id
// 之后能不能真的取回密钥」正是这次改动最需要被验证的那件事。
func credentialStores(cfg *config.Config, configDir string) {
	// 迁移提示：老 state.yaml 里若还有明文 api_key，加载后它仍会覆盖 cfg.LLM
	// （LoadStateInto merge 的是整个 Config），所以**不需要**为了兼容去读它；
	// 这里只是告诉用户「下次保存就不再写了」。
	if legacy, err := config.LegacyStateHasPlaintextKey(config.StatePath()); err == nil && legacy {
		logx.Warnf("state.yaml 仍含明文 api_key（兼容期内照常可用）；" +
			"下次保存设置后它将不再写入 —— 密钥只在 models.yaml / providers.yaml 与进程内存")
	}

	dir := configDir
	if d := cfg.ConfigDir(); d != "" {
		dir = d
	}
	if strings.TrimSpace(dir) == "" {
		logx.Warnf("未确定配置目录，跳过模型凭据解析（沿用 local.yaml / .env）")
		return
	}
	ms := config.NewModelStore(filepath.Join(dir, "models.yaml"))
	ps := config.NewProviderStore(filepath.Join(dir, "providers.yaml"))
	logx.Infof("%s", config.ResolveModelCredentials(cfg, ms, ps))
}
