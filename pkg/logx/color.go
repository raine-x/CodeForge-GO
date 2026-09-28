package logx

import "os"

// init 决定是否上色。三个否决项按优先级：
//
//	NO_COLOR  非空即关（社区约定，https://no-color.org）
//	TERM=dumb 终端明确表示不支持颜色
//	非终端    平台文件判定失败
//
// 平台判定在 initWindows / initUnix 里完成，VT 的开启也只能在那里做
// （需要控制台句柄），所以这里只负责环境变量与否决。
func init() {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		color = false
		return
	}
	color = detectTerminal()
}
