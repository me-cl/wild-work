// Command wild-work 系统托盘 daemon 入口。
// 单进程 = OpenAI 兼容 HTTP 服务 + 自动签到调度器 + 系统托盘 + 静态 Web UI。
// 双击 exe 启动常驻托盘；托盘菜单：打开主界面 / 查看日志 / 退出；
// 打开主界面或双击托盘 → 系统浏览器打开 Web UI。
//
// 装配逻辑在 internal/daemon（Android 入口 gomobile bind 复用同一份），本文件只负责
// 桌面特有的部分：工作目录解析、托盘、系统提示框、信号处理。
//
// 另有一个**非守护**入口：`--raccoon-callback <深链>`（小浣熊**旧版**桌面协议登录的回调，
// 由 Windows 按 HKCU 的 office-raccoon 注册表唤起）。该分支只把深链解析后落盘再退出，
// **绝不**启动第二份服务/托盘——详见 handleRaccoonCallback。
//
// 注：小浣熊登录已于 R44 改为**网页版 CDP 捕获**（不再改写注册表），此入口保留
// 仅为了接住旧版本可能残留的注册表指向，属兼容死代码，不再有新流程使用。
package main

import (
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/daemon"
	"wild-work/internal/platform"
	"wild-work/internal/raccoon"
	"wild-work/internal/systray"
	"wild-work/internal/webui"
)

//go:embed build/trayicon.ico
var trayIconICO []byte

func main() {
	// 工作目录：便携/CLI 形态固定为 exe 所在目录，保证相对路径配置（./auths ./data）稳定。
	// macOS .app bundle 内该目录只读、且随 app 替换被清空，故改用系统数据目录（见 workDir）。
	_ = os.Chdir(workDir())

	// 小浣熊授权登录的协议回调：由 Windows 按注册表命令行唤起本 exe。
	// 必须在**任何**初始化（config/日志/服务）之前拦下——它只落盘一个文件就退出，
	// 若走后面流程会启动第二份 daemon（日志与本进程抢锁、还可能在收到授权码后
	// 立刻把回调文件删掉），导致授权完成后账号加不进来。
	if handled, err := handleRaccoonCallback(os.Args[1:]); handled {
		if err != nil {
			log.Printf("小浣熊回调处理失败：%v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	cfgPath := "config.json"
	// --autostart 由开机自启项附带：开机启动不弹提示
	// --no-tray 无头模式（无桌面 Linux/服务器）
	autostart := false
	noTray := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--autostart":
			autostart = true
		case "--no-tray":
			noTray = true
		}
	}

	d, err := daemon.Start(daemon.Options{
		ConfigPath: cfgPath,
		WebFS:      webui.FS(),
	})
	if err != nil {
		// 启动失败必须终止：只记日志继续跑会让调度器与托盘照常起来，
		// 表现为「面板看着一切正常、其实是别的实例在服务本端口」，
		// 而两个进程共享同一数据目录，会重复签到并互相覆盖刷新出的 token。
		// （监听失败只是这条错误里最常见的一种，见 daemon.Start 的说明。）
		fatal("%v", err)
	}
	defer d.Stop()

	cfg := d.Config
	appInst := d.App

	// 端口可能由内核分配（WILDWORK_LISTEN=...:0）：横幅与通知都必须显示**实际**
	// 绑定端口，否则用户照着横幅连 `:0` 永远连不上。
	port := boundPort(d.Addr(), cfg.Listen.Port)

	// 启动提示（非 --autostart）：系统通知
	if !autostart && !noTray {
		platform.Notify("wild-work 已启动",
			fmt.Sprintf("OpenAI 兼容 API 地址：\nhttp://%s:%d\n\n点击右下角托盘图标或菜单打开主界面。", displayHost(cfg), port))
	}

	if noTray {
		// 无头模式：打印信息，阻塞等待信号
		fmt.Printf("wild-work headless mode started\n")
		fmt.Printf("  OpenAI API:    http://%s:%d/v1\n", displayHost(cfg), port)
		fmt.Printf("  API-Key:       %s\n", cfg.APIKey)
		fmt.Printf("  Web UI (本机): http://%s:%d/\n", openHost(cfg), port)
		fmt.Printf("  Listen:        %s\n", d.Addr())
		if appInst.PanelAuthEnabled() {
			fmt.Printf("  Panel auth:    admin_password enabled (cookie session, 7d)\n")
		}
		fmt.Printf("  Accounts:      workbuddy=%d traework=%d qoder=%d oczen=%d(匿名) kilo=%d(匿名) minimax=%d lobsterai=%d zcode=%d gemini=%d codearts=%d\n",
			d.Counts["workbuddy"], d.Counts["traework"], d.Counts["qoder"], d.Counts["oczen"], d.Counts["kilo"],
			d.Counts["minimax"], d.Counts["lobsterai"], d.Counts["zcode"], d.Counts["gemini"], d.Counts["codearts"])
		fmt.Printf("\nPress Ctrl+C to exit\n")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Printf("\nShutting down...\n")
		return
	}

	// 系统托盘（阻塞）。无桌面环境（如 SSH/服务会话）下托盘初始化失败时
	// 捕获 panic，提示用户使用 --no-tray 参数启动。
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "Tray init failed: %v\n", r)
				fmt.Fprintf(os.Stderr, "Use --no-tray to run in headless mode\n")
				os.Exit(1)
			}
		}()
		systray.Run(trayIconICO, "wild-work — 渠道聚合代理", systray.Actions{
			OpenUI: func() {
				// 本机打开面板：通配监听时浏览器访问不了 0.0.0.0，用局域网 IP 兑底
				_ = platform.OpenURL(fmt.Sprintf("http://%s:%d/", openHost(cfg), cfg.Listen.Port))
			},
			OpenLog: func() {
				if err := appInst.OpenLogFile(); err != nil {
					log.Printf("open log: %v", err)
				}
			},
			Quit: func() {
				if platform.AskYesNo("wild-work", "确定退出 wild-work 吗？") {
					// 先摘托盘图标：NIM_DELETE 由托盘消息循环执行，返回即已回收。
					// 直接 os.Exit 会跳过这一步，Windows 任务栏留下幽灵图标。
					systray.Quit(3 * time.Second)
				}
			},
		})
	}()

	// Run 返回 = 托盘图标已回收，停机清理（defer d.Stop）后进程自然退出。
}

// handleRaccoonCallback 处理 `--raccoon-callback <深链>` 这一**非守护**入口。
//
// 背景：小浣熊登录期间会把 HKCU 的 `office-raccoon` 协议命令行临时改成
//
//	"<wild-work.exe>" --raccoon-callback "%1"
//
// 用户授权完成后 Windows 按该命令行再拉起一份本 exe。该子进程的**唯一职责**
// 是把深链解析后原子落盘（raccoon.SaveCallback），由常驻 daemon 的轮询读取。
//
// 返回 handled=true 表示本次进程是回调子进程（调用方应直接退出）；
// err 非 nil 时仍属 handled（已接管，只是落盘失败）。
//
// state 目录取自 config.json 的 state_file 所在目录；config 读不到时退回
// raccoon 包的默认值（./data），与 config.Default 一致。
func handleRaccoonCallback(args []string) (handled bool, err error) {
	// 只认「第一个参数恰为 CallbackFlag」：避免把深链里的字符串误判成入口。
	if len(args) == 0 || args[0] != raccoon.CallbackFlag {
		return false, nil
	}

	stateDir := ""
	if cfg, cerr := config.Load("config.json"); cerr == nil && cfg != nil {
		stateDir = filepath.Dir(cfg.StateFile)
	}
	// Windows 的 windowsgui 构建**无控制台**，stderr 看不到——把结果追写到
	// data/app.log（与 daemon 同一文件、O_APPEND 个位数写入安全），否则回调失败时
	// 用户与运维都无任何线索（只能看到主进程 5 分钟后「登录超时」）。
	defer func() {
		msg := "raccoon 回调已落盘（等待主进程兑换 token）"
		if err != nil {
			msg = "raccoon 回调处理失败：" + err.Error()
		}
		appendCallbackLog(raccoon.StateDir(stateDir), msg)
	}()

	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return true, errors.New("缺少深链参数")
	}
	if serr := raccoon.SaveCallback(stateDir, args[1]); serr != nil {
		return true, serr
	}
	return true, nil
}

// appendCallbackLog 追写一行到 <stateDir>/app.log（失败静默，日志不能反过来阻断主流程）。
func appendCallbackLog(stateDir, msg string) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006/01/02 15:04:05.000000"), msg)
}

// workDir 解析数据目录（config.json / auths/ / data/ 的落点）。
//
//   - WILDWORK_HOME 显式指定时优先（测试/自定义布局/Android 用）；
//   - 运行在 macOS .app bundle 内（.../Xxx.app/Contents/MacOS）时改用
//     ~/Library/Application Support/WildWork：bundle 内容对普通用户只读，
//     且写到包内会在替换/升级 app 时连同账号一起丢失；
//   - 其余形态（CLI 二进制、Windows 便携包、Linux）沿用 exe 所在目录。
func workDir() string {
	if v := os.Getenv("WILDWORK_HOME"); v != "" {
		if err := os.MkdirAll(v, 0o755); err == nil {
			return v
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if strings.Contains(dir, ".app/Contents/MacOS") {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, "Library", "Application Support", "WildWork")
			if err := os.MkdirAll(p, 0o755); err == nil {
				return p
			}
		}
	}
	return dir
}

// displayHost 展示用途的主机名。
// 通配监听（0.0.0.0/::/空）是用户主动配置的意图，如实显示 0.0.0.0；
// localhost 归一为 127.0.0.1。不做任何「善意」替换——面板顶栏同理（见 app.js renderTopbar）。
// 注意：浏览器/托盘打开面板时用此值，0.0.0.0 在浏览器不可直接访问，
// 故 OpenUI 场景改用 lanIPOrLocalhost() 兑底（见 main 调用处）。
// boundPort 从**实际**监听地址里取端口，取不到时退回配置值。
// 端口写 0（内核分配）时配置里恒为 0，直接打印会让用户看到不可用的 `:0`。
func boundPort(listenAddr string, fallback int) int {
	if _, p, err := net.SplitHostPort(listenAddr); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func displayHost(cfg *config.Config) string {
	if cfg.Listen.Host == "" || cfg.Listen.Host == "::" {
		return "0.0.0.0"
	}
	if cfg.Listen.Host == "localhost" {
		return "127.0.0.1"
	}
	return cfg.Listen.Host
}

// lanIPOrLocalhost 返回局域网出口 IP，取不到时兑底 127.0.0.1。
// 用于「本机打开面板」场景：监听 0.0.0.0 时浏览器不能访问 0.0.0.0，
// 用局域网 IP（或 127.0.0.1）代替，用户可从本机或局域网其他机器访问。
func lanIPOrLocalhost() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() {
		return "127.0.0.1"
	}
	return addr.IP.String()
}

// openHost 返回适合「本机浏览器打开」的主机：通配监听时用局域网 IP 兑底。
func openHost(cfg *config.Config) string {
	switch cfg.Listen.Host {
	case "", "0.0.0.0", "::":
		return lanIPOrLocalhost()
	case "localhost":
		return "127.0.0.1"
	}
	return cfg.Listen.Host
}

// fatal 记录日志并弹出系统提示后退出。
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	platform.InfoBox("wild-work", msg)
	os.Exit(1)
}
