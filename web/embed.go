// Package web 通过 go:embed 内嵌前端静态资源，实现单二进制交付。
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var distFS embed.FS

// Dist 返回前端静态资源文件系统（根为 dist 目录）。
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
