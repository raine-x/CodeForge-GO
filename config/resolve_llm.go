// credentials.go 按 model id 解析出运行所需的凭据与端点。
//
// 为什么需要它：state.yaml 的 llm 段只存 `model: <id>`（见 StateLLM），
// 密钥与 base_url 必须在**启动时**从 models.yaml / providers.yaml 解析出来。
// 存派生值只会陈旧 —— 用户在设置页改了模型的上下文窗口或换了供应商，
// 落盘那份旧副本还在，按它建客户端就会连错地址。
//
// 核心原则：**store 有值才覆盖**。
// 解析不出密钥时保留 cfg 里已有的（来自 local.yaml.llm），
// 而不是清空。理由是兼容期：不少用户的密钥从来没进过模型库，只在
// local.yaml 或环境变量里 —— 那种情况下 store 查不到是正常的，
// 把它清空就等于「升级即登不上」。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ResolveModelCredentials 按 cfg.LLM.Model 在模型库里查到条目后，
// 把协议 / base_url / 显示名 / 上下文窗口 / 密钥补进 cfg.LLM。
//
// 返回值 msg 非空表示给调用方看的提示（已解析 / 未解析 / 为什么）。
// 幂等：重复调用结果相同。
//
// 只在 store 真的有值时覆盖 —— 见文件头的「store 有值才覆盖」。
func ResolveModelCredentials(cfg *Config, ms *ModelStore, ps *ProviderStore) string {
	if cfg == nil {
		return "配置为空"
	}
	id := strings.TrimSpace(cfg.LLM.Model)
	if id == "" {
		return "未选择模型"
	}
	if ms == nil {
		return "模型库未就绪"
	}
	entry, ok := ms.Find(id)
	if !ok {
		// 不是模型库里的 id —— 可能是用户直接在 local.yaml 写的内联模型名。
		// 这时**什么都不动**：那套配置本来就不依赖模型库。
		return "模型 " + id + " 不在模型库中，沿用现有配置"
	}
	e := ResolveModel(entry, ps)

	// 端点与展示名：store 有值才覆盖
	if v := strings.TrimSpace(e.BaseURL); v != "" {
		cfg.LLM.BaseURL = v
	}
	if v := strings.TrimSpace(e.Protocol); v != "" {
		cfg.LLM.Provider = v
	}
	if v := strings.TrimSpace(entry.DisplayName()); v != "" {
		cfg.LLM.DisplayName = v
	}
	// ⚠️ 刻意**不动** MaxTokens：它已在 applyModelEntry 时按模型的 ctx_out 写入，
	// 用户还能在设置页手动覆盖。持久化下来的那份才是权威 ——
	// 这里再按模型条目盖一次，用户的手动设置就白改了。

	// 多模态能力声明：整体覆盖（含 nil）。
	//
	// ⚠️ 必须是**无条件赋值**，不能写成「条目声明了才覆盖」——
	// 那是「store 有值才覆盖」原则在这里的例外：换模型时若新条目没声明
	// vision/video 而旧模型声明过，不清空就会把**上一个模型的能力**扣在
	// 新模型头上（症状：换到纯文本模型，图片仍在静默被拒）。
	// 能力声明是模型的内在属性，不存在「继承上一任」这回事。
	cfg.LLM.Vision = entry.Vision
	cfg.LLM.Video = entry.Video

	if key := resolveModelKey(e); key != "" {
		cfg.LLM.APIKey = key
		return "已从模型库解析 " + id
	}
	if strings.TrimSpace(cfg.LLM.APIKey) != "" {
		// 模型库没给密钥，但配置里已有（local.yaml / 环境变量）→ 沿用它。
		return "模型 " + id + " 未在模型库存密钥，沿用环境变量/local.yaml 中的配置"
	}
	return "⚠️ 模型 " + id + " 未能取到密钥，可能无法调用"
}

// resolveModelKey 按模型条目的 key_source 取密钥：env 走环境变量，plain 取内联值。
//
// ⚠️ 解析不出就返回空串，**不要**在这里兜一个空串给调用方写进 cfg ——
// 那会把「查不到」变成「清空」。
func resolveModelKey(e ModelEntry) string {
	if strings.EqualFold(strings.TrimSpace(e.KeySource), "env") {
		if name := strings.TrimSpace(e.KeyName); name != "" {
			return strings.TrimSpace(os.Getenv(name))
		}
	}
	return strings.TrimSpace(e.KeyValue)
}

// LegacyStateHasPlaintextKey 报告旧的 state.yaml 里是否还留着明文 api_key。
//
// 只用于**打一条迁移提示**：不删不改。真正的迁移是自然发生的 ——
// 加载后老键仍会合进 cfg.LLM（LoadStateInto merge 的是整个 Config），
// 而下一次 SaveState 起就只写 model 了。
func LegacyStateHasPlaintextKey(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("读取状态文件失败: %w", err)
	}
	var probe struct {
		LLM struct {
			APIKey string `yaml:"api_key"`
		} `yaml:"llm"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false, err
	}
	return strings.TrimSpace(probe.LLM.APIKey) != "", nil
}
