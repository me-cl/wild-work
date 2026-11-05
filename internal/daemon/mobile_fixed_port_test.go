package daemon

// 移动端（gomobile 壳，mobile/mobile.go）必须使用**固定端口**。
//
// 这条为什么需要静态断言：端口是不是固定的，编译期没有任何信号。早期实现往进程里
// 塞 `WILDWORK_LISTEN=127.0.0.1:0` 让内核随机分配，症状是「每次重启面板地址都变」——
// WebView 自己会从 Mobile.url() 取地址，所以肉眼几乎看不出来，只有用户把地址记进
// 客户端 / 做 adb forward 时才发现连不上。改回固定端口后，谁再往那儿加一行 Setenv
// 也不会有任何报错。
//
// 判据用「注释剥离后的代码」而不是裸字符串：mobile.go 的注释里**必须**能写明
// 「不要设 WILDWORK_LISTEN」，按裸字符串判会把正确的注释判成违规。

import (
	"os"
	"strings"
	"testing"

	"wild-work/internal/config"
)

// stripLineComments 去掉 `//` 到行尾的内容。
//
// 只处理行注释（mobile.go 没有块注释）；也避免误伤字符串里的 `//`——本文件
// 关心的两个模式（`os.Setenv(` / `WILDWORK_LISTEN`）都不出现在 URL 一类字符串里，
// 而真正要防的误判来源就是注释，够用即可。
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, ln := range lines {
		if j := strings.Index(ln, "//"); j >= 0 {
			lines[i] = ln[:j]
		}
	}
	return strings.Join(lines, "\n")
}

func mobileCode(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../mobile/mobile.go")
	if err != nil {
		t.Fatalf("读 mobile/mobile.go 失败: %v", err)
	}
	return stripLineComments(string(raw))
}

// 移动端不得通过环境变量改写监听：固定端口的唯一来源是 config.json。
//
// 为什么连「设成 17863」也不允许：applyEnv 的优先级高于配置文件（见
// config.applyEnv），写死任何值都会把用户在面板「设置 → 监听」里改的地址
// 永久压掉——设置项看着还在，重启后却悄悄失效。
func TestMobileMustNotOverrideListenEnv(t *testing.T) {
	code := mobileCode(t)
	if strings.Contains(code, "Setenv(") {
		t.Fatal("mobile/mobile.go 不得调用 os.Setenv：移动端端口固定，环境变量的优先级高于 " +
			"config.json，写死会让面板里改的监听地址在重启后失效")
	}
	if strings.Contains(code, "WILDWORK_LISTEN") {
		t.Fatal("mobile/mobile.go 的**代码**里不得出现 WILDWORK_LISTEN（注释里说明「不要设」是允许的）")
	}
}

// 固定端口的运行期判据：手机上的全新安装落到 config.DefaultPort，而不是 0。
//
// 与上面那条互为补充：上面守「壳没有偷偷传 env」，这条守「不传 env 时确实是固定值」。
// 两条都绿才说明「每次启动端口一致」这个结论成立。
func TestMobileFreshInstallLandsOnDefaultPort(t *testing.T) {
	// 壳不再设 env，等价于「环境里没有 WILDWORK_LISTEN」。
	t.Setenv("WILDWORK_LISTEN", "")

	fresh := t.TempDir() + "/config.json"
	c, created, err := config.LoadOrCreate(fresh)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !created {
		t.Fatal("配置不存在时应报告 created=true")
	}
	if c.Listen.Ephemeral {
		t.Fatalf("移动端全新安装不得是随机端口：%+v", c.Listen)
	}
	const want = "127.0.0.1:17863"
	if got := c.Listen.Addr(); got != want {
		t.Fatalf("移动端全新安装应监听 %s，实际 %s", want, got)
	}

	// 第二次启动必须落到同一个端口（读回刚写下的 config.json）。
	again, created, err := config.LoadOrCreate(fresh)
	if err != nil {
		t.Fatalf("第二次 LoadOrCreate: %v", err)
	}
	if created {
		t.Fatal("配置文件已存在时不得再报 created")
	}
	if again.Listen.Addr() != want {
		t.Fatalf("重启后端口变了：%s -> %s", want, again.Listen.Addr())
	}
}
