//go:build android

package platform

import (
	"os"
)

// Android 实现：没有 shell、没有 xdg-open、没有桌面会话。
//
// 关键约定（与 platform_other.go 同形，别处的调用方不必分支）：
//   - OpenURL / OpenWithTextEditor **不报错地什么都不做**：Android 外壳的 WebView
//     自己就能打开面板，而登录链里的「打开授权页」由外壳接管（见 mobile 包）。
//     返回 error 会让 app 的登录流程把它当成用户可见的失败。
//   - Notify / InfoBox 打到 stderr（Android 上即 logcat），便于 adb logcat 排查。
//   - AskYesNo 恒 false（没有确认框；退出由外壳的返回键逻辑处理）。
//   - 开机自启无对应概念（由外壳的进程生命周期决定）。

// InfoBox Android：输出到 stderr（logcat）。
func InfoBox(title, msg string) { Notify(title, msg) }

// Notify Android：输出到 stderr（logcat）。
func Notify(title, msg string) {
	_, _ = os.Stderr.WriteString(title + ": " + msg + "\n")
}

// AskYesNo Android：默认否。
func AskYesNo(title, msg string) bool { return false }

// DefaultBrowserIncognito Android：不探测（无桌面浏览器）。
func DefaultBrowserIncognito() (exePath, flag string, ok bool) { return "", "", false }

// LaunchIncognito Android：无操作（授权页由 WebView 外壳承载）。
func LaunchIncognito(exe, flag, url string) error { return nil }

// OpenURL Android：无操作（面板就在外壳的 WebView 里）。
func OpenURL(url string) error { return nil }

// SetAutostart Android：未实现。
func SetAutostart(on bool) error { return nil }

// AutostartEnabled Android：false。
func AutostartEnabled() bool { return false }

// OpenWithTextEditor Android：无操作（日志在应用私有目录，用「查看日志」接口取文本）。
func OpenWithTextEditor(path string) error { return nil }
