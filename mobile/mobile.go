// Package mobile 供 gomobile bind 生成 AAR，让 Android 外壳（Kotlin/Java）
// 在进程内启动 wild-work 的常驻服务。
//
// 与桌面入口的关系：装配逻辑在 internal/daemon，两边共用同一份——桌面端额外有
// 系统托盘与信号处理，Android 端没有，且必须由前台服务保证进程不被回收。
//
// 构建：直接用 `shell/android/build-aar.sh`（它处理了嵌套模块与 NDK 版本）。
// 等价命令（须在**本目录**执行——本包是独立的嵌套模块，见 mobile/go.mod）：
//
//	gomobile bind -target=android/arm64,android/arm \
//	    -androidapi 26 -javapkg com.wildwork -o ../build/android/wildwork.aar .
//
// gomobile 只能导出**受支持的类型**：字符串/数字/布尔/字节切片/这些类型的切片，
// 以及能用 newXxx 构造的结构体。故这里只暴露字符串与基本类型，错误以
// （错误字符串, 是否成功）两段式回传——返回 error 接口无法跨 JNI。
package mobile

import (
	"fmt"
	"log"
	"sync"
	"time"

	"wild-work/internal/daemon"
	"wild-work/internal/webui"
)

var (
	mu  sync.Mutex
	cur *daemon.Daemon
)

// Start 在 home 目录（通常是 Context.getFilesDir()）下启动服务，返回可访问的
// 面板 URL（形如 http://127.0.0.1:17863/）。端口是**固定**的：首次安装取
// config.DefaultPort，之后跟随 config.json 里用户改过的值，重启不变（见 startLocked）。
//
// 返回空串 = 启动失败，具体原因可从 Err() 取（沿 LastError 语义）。
// 为什么不用 (string, error)：gomobile 的绑定生成器不支持 error 接口，
// 它会让整个绑定失败而不是回传错误。
func Start(home string) (url string) {
	mu.Lock()
	defer mu.Unlock()
	lastErr = ""

	if cur != nil {
		// 已在运行：返回当前地址（重复切换 Activity 不应重启服务）。
		return cur.URL()
	}

	d, err := startLocked(home)
	if err != nil {
		lastErr = err.Error()
		log.Printf("mobile: 启动失败: %v", err)
		return ""
	}
	cur = d
	return d.URL()
}

func startLocked(home string) (*daemon.Daemon, error) {
	// 监听地址完全交给 config.json：全新安装是 127.0.0.1 + config.DefaultPort
	// （17863），用户在面板「设置 → 监听」里改过就沿用改后的值。
	//
	// **不要**在这里设置 WILDWORK_LISTEN（任何值，包括改成固定的 17863）：
	// applyEnv 的优先级高于配置文件（见 config.applyEnv），写死就等于把用户
	// 在面板里改的监听地址永久压掉——设置项看着还在，重启后却悄悄失效。
	//
	// 也不要再要求内核随机分配（`:0`）：早期实现设过
	// WILDWORK_LISTEN=127.0.0.1:0，结果是**每次重启面板端口都变**，adb forward
	// 与「把面板地址记进手机上的客户端」都做不了；WebView 本来就从 Mobile.url()
	// 取地址，随机端口换不来任何好处。
	// 端口被占用时 daemon.Start 会返回错误（R68），不会自选下一个端口。
	return daemon.Start(daemon.Options{
		Home:       home,
		ConfigPath: "config.json",
		WebFS:      webui.FS(),
		// 面板的「退出」只停服务：进程属于宿主 App，os.Exit 会连带杀掉界面；
		// 而且前台服务是 START_STICKY，不主动停会被系统立刻拉回来。
		Embedded: true,
		OnQuit: func() {
			mu.Lock()
			cur = nil
			mu.Unlock()
		},
	})
}

var lastErr string

// Err 返回最近一次 Start 失败的原因（空串 = 无错误）。
func Err() string {
	mu.Lock()
	defer mu.Unlock()
	return lastErr
}

// Stop 停止服务（幂等）。外壳应在 Service.onDestroy 里调用。
func Stop() {
	mu.Lock()
	d := cur
	cur = nil
	mu.Unlock()
	if d != nil {
		d.Stop()
	}
}

// Running 报告服务是否在运行。
func Running() bool {
	mu.Lock()
	defer mu.Unlock()
	return cur != nil
}

// URL 返回当前面板地址（未运行时空串）。
func URL() string {
	mu.Lock()
	defer mu.Unlock()
	if cur == nil {
		return ""
	}
	return cur.URL()
}

// Version 返回版本号（外壳的「关于」页用；与桌面同一常量来源）。
func Version() string {
	return daemon.Version()
}

// WaitReady 在 timeoutMs 毫秒内轮询，直到服务可访问或超时。
//
// 为什么要它：Start 返回时监听已经建立（net.Listen 成功才返回），但 Android 的
// 前台服务与 WebView 是两条独立的启动路径，外壳若在 Start 之前就 loadUrl 会拿到
// 「连接被拒绝」。用它给外壳一个确定的等待点，比在外壳里 sleep 可靠。
func WaitReady(timeoutMs int) bool {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		if URL() != "" {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return URL() != ""
}

// String 便于调试：把当前状态拼成一行的说明。
func Status() string {
	mu.Lock()
	defer mu.Unlock()
	if cur == nil {
		if lastErr != "" {
			return "stopped: " + lastErr
		}
		return "stopped"
	}
	return fmt.Sprintf("running at %s", cur.URL())
}
