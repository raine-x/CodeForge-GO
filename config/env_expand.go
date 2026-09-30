package config

import (
	"os"
	"sort"
	"strings"

	"codeforge/pkg/logx"
)

// 环境变量展开：YAML 里可以写 `${VAR}` 或 `$VAR`，值从进程环境取。
//
// 为什么需要它：密钥不该躺在配置文件里。`providers.yaml` / `models.yaml` /
// `local.yaml` 都在 .gitignore 里，但 .gitignore 挡不住同步盘、备份脚本、
// 打包上传。而环境变量不会跟着文件走。
//
// 用法：
//
//	# config/providers.yaml
//	providers:
//	  - id: my-gateway
//	    base_url: ${GATEWAY_URL}/v1
//	    key_source: env
//	    key_name: ${GATEWAY_KEY_NAME}   # 变量名本身也可以是变量（少见但合法）
//	    key_value: ${GATEWAY_KEY}       # 明文位写变量引用，文件里就没有密钥了
//
// 为什么在 Unmarshal **之前**处理原始字节，而不是给每个字段加解析：
//
//   - 一次处理覆盖所有字段，将来加字段自动支持，不用记得补；
//   - 免去「这个字段支不支持展开」的知识负担 —— 现在仍有字段不支持，
//     而那种遗漏只在特定部署下才暴露。
//
// 未设置的变量怎么处理 —— 替换成空串，**并记录下来**：
//
// 留成空串而不是保留 `${VAR}` 字面量：后者会变成一把字面量是 "${GATEWAY_KEY}"
// 的密钥，去请求上游得到 401，而错误信息完全指错方向。
//
// 也不直接报错终止启动：那会让「配了十项、错了一项」的配置文件整个不可用，
// 用户连别的配置都读不到。改为替换成空 + 在启动日志里点名未设置的变量，
// 由既有的「未能取到密钥」路径给出面向用户的提示。
type missingEnvVar struct {
	name string
}

// expandEnvYAML 展开 YAML 字节里的环境变量引用，返回处理后的字节与未设置的变量名。
//
// 未设置的变量会被**记录**但**不阻止**加载 —— 见类型注释里的理由。
func expandEnvYAML(data []byte) ([]byte, []string) {
	if !strings.Contains(string(data), "$") {
		return data, nil // 绝大多数文件走这条快路径
	}
	text := string(data)
	missing := map[string]bool{}

	var b strings.Builder
	b.Grow(len(text))

	for i := 0; i < len(text); {
		// 转义：`\${VAR}` 表示字面量 ${VAR}。
		// 反斜杠被**丢弃**（与 shell 的 `\${…}` 语义一致）——
		// 留着它的话 YAML 里会看到 `\${VAR}`，而用户以为自己已经转义过了。
		if text[i] == '\\' && i+1 < len(text) && text[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if text[i] != '$' {
			b.WriteByte(text[i])
			i++
			continue
		}
		name, n, ok := parseEnvRef(text[i:])
		if !ok {
			b.WriteByte(text[i])
			i++
			continue
		}
		val, present := os.LookupEnv(name)
		if !present {
			missing[name] = true
		}
		b.WriteString(val)
		i += n
	}

	out := make([]string, 0, len(missing))
	for k := range missing {
		out = append(out, k)
	}
	sort.Strings(out)
	return []byte(b.String()), out
}

// parseEnvRef 在 s 以 '$' 开头处解析一个变量引用，返回变量名与消耗的字节数。
//
// 支持两种写法：
//
//	${NAME}   花括号，边界明确，推荐
//	$NAME     裸形式，名字取 [A-Za-z_][A-Za-z0-9_]*
//
// 裸形式遇到非标识符字符就停止 —— `$5` 不是变量（避免把价格里的 $ 吃掉）。
func parseEnvRef(s string) (name string, n int, ok bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", 0, false
	}
	if s[1] == '{' {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return "", 0, false // 没闭合，当普通文本
		}
		name = s[2:end]
		if !isEnvName(name) {
			return "", 0, false
		}
		return name, end + 1, true
	}
	// 裸形式
	i := 1
	for i < len(s) && isEnvNameByte(s[i], i == 1) {
		i++
	}
	if i == 1 {
		return "", 0, false // $ 后面不是标识符首字符
	}
	return s[1:i], i, true
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isEnvNameByte(s[i], i == 0) {
			return false
		}
	}
	return true
}

func isEnvNameByte(c byte, first bool) bool {
	switch {
	case c == '_':
		return true
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return !first // 标识符不能以数字开头
	}
	return false
}

// reportMissingEnvVars 在启动日志里点名「配置引用了但没设置」的环境变量。
//
// 单列一条警告而不是混在别处：这类问题的表现是「明明配了却连不上」，
// 而错误信息通常指向 401 / 鉴权失败，完全看不出是变量没设。
func reportMissingEnvVars(where string, names []string) {
	if len(names) == 0 {
		return
	}
	logx.Warnf("%s 引用了未设置的环境变量：%s（这些值已按空处理；"+
		"若本该有值，请检查是否 export/设过了）", where, strings.Join(names, ", "))
}
