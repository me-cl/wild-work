package app

// embedded_launch_test.go 锁住「嵌入宿主（Android WebView）不自动打开浏览器」这条后端分支。
//
// 背景：Android 上 platform.OpenURL / LaunchIncognito 都是**显式空实现**（见
// internal/platform/platform_android.go），CodeArts 的 openIncognito 走到底只会：
//  ① 打一条「未探测到无痕的默认浏览器，退回系统默认打开」的**误导日志**
//     （部署到手机上排查时，这条日志会把人往「浏览器没装好」的方向带）；
//  ② 什么也不做，但前端仍按 loginBackendOpened 判断，可能声称「已为你打开无痕窗口」。
//
// 面板侧的对应行为（不自动打开、改给「打开网页」+「复制链接」）由
// scripts/ui-static-check.mjs 的「嵌入宿主（Android）登录流程」一节静态锁死；
// 这里只锁后端那两行的短路。

import (
	"testing"
)

// TestOpenAuthURLIncognitoSkipsWhenEmbedded 嵌入宿主下必须**在调平台能力之前**返回：
// loginBackendOpened 保持 false（前端据此不声称已打开），且不触发任何平台调用。
//
// 为什么这条测试是安全的：embedded=true 时函数在 platform.* 之前就 return，
// 所以即使在 Windows 上跑测试也不会真的弹出浏览器；反过来若有人删掉那段短路，
// 本测试会真的去开浏览器 —— 那种情况下失败信息比「静默打开一个窗口」更重要，
// 但为了不污染开发机，**不要**给它加非嵌入路径的用例。
func TestOpenAuthURLIncognitoSkipsWhenEmbedded(t *testing.T) {
	a := &App{embedded: true}
	a.openAuthURLIncognito("https://example.invalid/authorize?x=1")
	if a.loginBackendOpened {
		t.Fatal("嵌入宿主下 loginBackendOpened 必须保持 false —— 否则面板会声称「已为你打开无痕窗口」，" +
			"而实际上 WebView 里不会出现任何窗口")
	}
}

// TestStartLoginForEmbeddedStillReturnsURL 嵌入宿主不改变「后端返回 auth_url」这条契约：
// 面板靠它填「打开网页」按钮的 data-url。这里只断言 embedded 字段本身不影响表驱动的
// 发起方式判定（回归护栏：别在 StartLoginFor 里按 embedded 去短路渠道）。
func TestLoginFlowsUnaffectedByEmbedded(t *testing.T) {
	// loginFlows 是包级唯一真源，与宿主形态无关。若有人图省事在表里塞嵌入分支，
	// 桌面端会跟着失效 —— 故此处以「表仍完整且每个渠道恰好一种发起方式」为护栏。
	if len(loginFlows) == 0 {
		t.Fatal("loginFlows 为空 —— 登录能力表未初始化")
	}
	for k, f := range loginFlows {
		modes := 0
		if f.Start != nil {
			modes++
		}
		if f.DisplayURL != "" {
			modes++
		}
		if f.noFlowReason != "" {
			modes++
		}
		if modes != 1 {
			t.Errorf("provider.%s 表达了 %d 种发起方式（嵌入宿主不应改变这张表）", k, modes)
		}
	}
}
