package daemon

import (
	"os"
	"strings"
	"testing"
)

// 本测试锁定一个**真实 bug**（2026-10-15 发现）：
//
//	启动时 `appInst.StartServer()` 失败（最常见：端口已被另一个实例占用）
//	原实现只 `log.Printf` 一行就继续往下跑 —— 调度器与托盘照常起来，
//	表现为「面板看着一切正常、其实是别的实例在服务本端口」，
//	而两个进程共享同一数据目录（config.json / auths / data），
//	会重复签到并互相覆盖刷新出的 token。
//
// 为什么用静态检查而不是运行时测试：
// 触发这条分支必须真实占用端口、再观测**进程退出**，单测里做不到；
// 而「失败分支有没有把失败传出去」在源码层面就能判定。
//
// 装配层从 cmd/wild-work 抽到 internal/daemon 后（Android 入口复用），判据拆成两半：
//
//	① daemon.Start 必须把监听失败**返回**给调用方（不得只记日志后继续跑）；
//	② 桌面入口拿到错误必须 fatal（不得 log 后继续进托盘循环）。
//
// 只断言其中一半都会漏掉回退：只返回不终止 = 进程带着失败继续跑；
// 只终止不返回 = Android 侧拿到 nil 当成功。
func TestListenFailureIsReturned(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatalf("读 daemon.go 失败: %v", err)
	}
	code := string(src)

	const marker = "appInst.StartServer(); err != nil {"
	i := strings.Index(code, marker)
	if i < 0 {
		t.Fatalf("未在 daemon.go 中找到 %q（代码结构已变？）", marker)
	}

	block := braceBlock(t, code[i+len(marker)-1:])
	if !strings.Contains(block, "return nil, fmt.Errorf(") {
		t.Errorf("❌ StartServer 失败分支没有把错误返回给调用方，进程会带着「监听失败」继续跑：\n"+
			"   调度器与托盘照常启动，面板却由别的实例服务；两进程共享数据目录，\n"+
			"   会重复签到并互相覆盖刷新出的 token。\n"+
			"   失败分支源码：\n%s", block)
	}
}

// TestDesktopEntryFatalOnStartFailure 锁桌面入口：daemon.Start 的错误必须交给 fatal()。
//
// 判据不是「出现 fatal」——main.go 里 fatal 只有这一处调用点，出现即说明启动失败会
// 弹框并退出；若将来有人改成 `log.Printf` 后继续，这条会红。
func TestDesktopEntryFatalOnStartFailure(t *testing.T) {
	src, err := os.ReadFile("../../cmd/wild-work/main.go")
	if err != nil {
		t.Fatalf("读 cmd/wild-work/main.go 失败: %v", err)
	}
	code := string(src)

	const marker = "daemon.Start(daemon.Options{"
	i := strings.Index(code, marker)
	if i < 0 {
		t.Fatalf("未在 main.go 中找到 %q（入口结构已变？）", marker)
	}
	// 从调用点往后找该调用所在的语句块（`if err != nil {` 紧随其后）
	tail := code[i:]
	j := strings.Index(tail, "err != nil {")
	if j < 0 {
		t.Fatalf("daemon.Start 之后未找到 err != nil 分支")
	}
	block := braceBlock(t, tail[j+len("err != nil "):])
	if !strings.Contains(block, "fatal(") {
		t.Errorf("❌ daemon.Start 失败分支未调用 fatal()，桌面进程会继续进托盘循环：\n%s", block)
	}
}

// braceBlock 从 `{` 起按花括号配平截取一个完整代码块（含首尾花括号）。
func braceBlock(t *testing.T, s string) string {
	t.Helper()
	depth := 0
	for i, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	t.Fatalf("花括号不配平，无法截取代码块")
	return ""
}
