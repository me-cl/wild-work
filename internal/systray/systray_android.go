//go:build android

// Package systray 的 Android 实现：全部空操作。
//
// 为什么必须在包内切成两半而不是在调用方加 if：Android 根本没有系统托盘，
// 但「Quit」这个语义仍被 internal/app 的 Web UI「退出」按钮使用（它调用
// systray.Quit 后再 os.Exit）。若整个包在 Android 下缺席，app 包就编不过；
// 若让 energye/systray 编进来，它会在 dbus 初始化处 panic，且把 cgo 拖进
// gomobile 的构建面。
//
// 语义：Quit 恒返回 true（没有图标要回收），Run 立即返回（没有消息循环）。
package systray

import "time"

// Actions 托盘动作回调（Android 下不会被调用，仅为保持接口同形）。
type Actions struct {
	OpenUI  func()
	OpenLog func()
	Quit    func()
}

// Run 在 Android 下立即返回：没有托盘可开。
func Run(icon []byte, tooltip string, act Actions) {}

// Quit 在 Android 下恒为「已回收」：没有图标需要摘除。
func Quit(timeout time.Duration) bool { return true }
