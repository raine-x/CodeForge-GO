package store

import "time"

// timeParse 集中处理 RFC3339 解析（迁移代码用）。
func timeParse(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

// timeOrNow 时间戳为 0 时回退当前时间（旧文件缺字段兜底）。
func timeOrNow(n int64) time.Time {
	if n > 0 {
		return time.Unix(n, 0)
	}
	return time.Now()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
