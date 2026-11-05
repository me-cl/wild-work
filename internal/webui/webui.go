// Package webui 以 embed 形式携带面板静态资源（纯静态，无构建链）。
//
// 单独成包而不是留在 cmd/wild-work 里：桌面入口与 Android（gomobile bind）
// 入口都要用同一份资源，而 embed 指令不允许引用包目录之外的文件。
// 两份副本比多一层包贵得多——任一份漏改就是「两个客户端显示不同的面板」。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:web
var assets embed.FS

// FS 返回 Web UI 的根文件系统（路径形如 index.html、app.js）。
//
// 失败只可能来自包内常量错误（embed 目录名被改），属编译期就该发现的问题，
// 故直接 panic——返回 error 只会让每个调用方重复一遍不可能发生的分支。
func FS() fs.FS {
	sub, err := fs.Sub(assets, "web")
	if err != nil {
		panic("webui: embed web: " + err.Error())
	}
	return sub
}
