// Package daemon 装配并启动 wild-work 的常驻服务：HTTP 服务（三接口 + Web UI +
// 管理 API）+ 各渠道调度器 + 积分/费率自动刷新 + 运行统计喂入。
//
// 为什么从 cmd/wild-work/main.go 抽出来：Android 端（gomobile bind 出的 AAR）
// 没有「进程入口」这个概念，它需要的是一个可被 Java/Kotlin 调用的 Start/Stop；
// 而桌面端仍要托盘与信号处理。两边共用同一份装配逻辑，装配层的护栏测试
// （「每个渠道的调度器都启动了」「每个渠道的 client 都进了 dump 表」这类静态断言）
// 也才能继续以源码为判据，而不是被拆成两套各自漂移的接线。
//
// 本包只做装配与生命周期；托盘、浏览器打开、信号处理留在各平台入口。
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wild-work/internal/app"
	"wild-work/internal/auth"
	"wild-work/internal/cline"
	"wild-work/internal/codearts"
	"wild-work/internal/comate"
	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/devdump"
	"wild-work/internal/gateway"
	"wild-work/internal/gemini"
	"wild-work/internal/glm"
	"wild-work/internal/kilo"
	"wild-work/internal/ledger"
	"wild-work/internal/lingxi"
	"wild-work/internal/lobsterai"
	"wild-work/internal/loomy"
	"wild-work/internal/loopguard"
	"wild-work/internal/minimax"
	"wild-work/internal/monkeycode"
	"wild-work/internal/oczen"
	"wild-work/internal/officeace"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/qoder"
	"wild-work/internal/qodercn"
	"wild-work/internal/qodercom"
	"wild-work/internal/qwenwork"
	"wild-work/internal/raccoon"
	"wild-work/internal/scheduler"
	"wild-work/internal/server"
	"wild-work/internal/stats"
	"wild-work/internal/traework"
	"wild-work/internal/upstream"
	"wild-work/internal/workbuddyai"
	"wild-work/internal/zcode"
)

// Version 返回版本号（单一来源仍是 internal/app 的常量）。
func Version() string { return app.Version }

// Options 是启动所需的全部外部输入（其余一律从配置文件与磁盘推出）。
type Options struct {
	// Home 数据目录（config.json / auths/ / data/ 的落点）。非空时先 chdir 过去，
	// 让配置里的相对路径（./auths、./data）稳定；空则沿用当前目录。
	Home string
	// ConfigPath 配置文件名，空则 "config.json"。
	ConfigPath string
	// WebFS 面板静态资源根（形如 index.html、app.js 已就位）。必填——两套入口
	// （桌面 / Android）用的是同一份 internal/webui。
	WebFS fs.FS
	// Embedded 表示宿主把服务嵌进了自己的进程（Android 壳）。影响唯一一件事：
	// 面板的「退出」只停服务、不 os.Exit（进程是宿主的，杀掉会连带灭掉界面）。
	Embedded bool
	// OnQuit 在 Embedded 模式下、面板点「退出」且服务已停之后调用，供宿主收尾
	// （Android 用它停前台服务）。非 Embedded 时忽略。
	OnQuit func()
}

// Daemon 是已启动的常驻服务句柄。
type Daemon struct {
	Config *config.Config
	App    *app.App

	// Counts 是各渠道启动时载入的账号数（--no-tray 启动信息用）。
	// key 是渠道名（provider.Kind.String()）；匿名渠道也计入（值恒为 1）。
	Counts map[string]int

	addr string

	stop context.CancelFunc
	once sync.Once
}

// Addr 返回**当前**实际监听地址（形如 127.0.0.1:17863）。
//
// 每次都问 App 而不是返回启动时快照：面板可以改监听端口（设置 → 全局监听），
// 端口也可写 0 让内核分配——快照会让 Android 外壳打开一个已经不对的 URL。
func (d *Daemon) Addr() string {
	if d.App != nil {
		if a := d.App.ListenAddr(); a != "" {
			return a
		}
	}
	return d.addr
}

// URL 返回本机可用浏览器打开的面板地址。
func (d *Daemon) URL() string { return "http://" + openHostFor(d.Addr()) + "/" }

// openHostFor 把监听地址变成「本机浏览器能访问」的地址：通配地址换成环回。
//
// WebView / 托盘打开面板永远发生在同一台机器上，0.0.0.0 与 [::] 在浏览器里
// 不可直接访问，必须换成 127.0.0.1。
func openHostFor(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Stop 停止调度器并关闭 App（幂等）。
func (d *Daemon) Stop() {
	if d == nil {
		return
	}
	d.once.Do(func() {
		if d.stop != nil {
			d.stop()
		}
		// App.Stop 内部已经 Close（见 internal/app），不要再调一次。
		if d.App != nil {
			d.App.Stop()
		}
	})
}

// Start 完成全部装配并启动服务。任何一步失败都返回 error——**不在本包里 os.Exit**：
// 桌面入口要弹系统提示框后退出，Android 入口要把错误回传成 Java 异常，两者的
// 处置方式不同，装配层只负责如实报告。
func Start(opts Options) (*Daemon, error) {
	if opts.ConfigPath == "" {
		opts.ConfigPath = "config.json"
	}
	if opts.WebFS == nil {
		return nil, fmt.Errorf("daemon: WebFS 未提供")
	}
	if opts.Home != "" {
		if err := os.MkdirAll(opts.Home, 0o755); err != nil {
			return nil, fmt.Errorf("创建工作目录 %s 失败：%w", opts.Home, err)
		}
		if err := os.Chdir(opts.Home); err != nil {
			return nil, fmt.Errorf("切换工作目录 %s 失败：%w", opts.Home, err)
		}
	}

	// LoadOrCreate 而不是 Load + 手工兜底：全新安装也必须吃 WILDWORK_* 环境变量。
	// Android 外壳用 `WILDWORK_LISTEN=127.0.0.1:0` 要求内核分配端口，首次启动时
	// config.json 还不存在——早先这里走 `config.Default()` 直接跳过了 applyEnv，
	// 外壳拿到的端口与真实监听不一致（2026-11-05 实测发现）。
	cfg, created, err := config.LoadOrCreate(opts.ConfigPath)
	if err != nil && cfg == nil {
		return nil, fmt.Errorf("加载配置失败：%v\n\n请检查 config.json 后重新启动", err)
	}
	if err != nil {
		// 配置建出来了但落盘失败：不阻塞启动（内存里的配置是对的），只记日志。
		log.Printf("config.json 不存在，使用默认配置（写盘失败：%v）", err)
	} else if created {
		log.Printf("config.json 不存在，使用默认配置并生成")
	}
	// 管理面板鉴权：监听非环回地址（0.0.0.0 / 网卡 IP）时必须设置管理员密码，
	// 否则局域网任何设备都能无凭据打开面板、退出程序。启动层与面板保存层双重把关。
	if !cfg.Listen.IsLoopback() && strings.TrimSpace(cfg.AdminPass) == "" {
		return nil, fmt.Errorf("监听 %s 已对外暴露，但 config.json 中未设置 admin_password。\n\n"+
			"请设置 admin_password（或在面板设置中修改监听地址）后重新启动；\n"+
			"若只想本机使用，把 listen.host 改回 127.0.0.1。", cfg.Listen.Addr())
	}
	if cfg.AdminPass != "" && len(cfg.AdminPass) < 8 {
		log.Printf("警告：管理员密码不足 8 位，建议使用更长口令（面板鉴权已生效）")
	}
	// 思考链死循环检测（发癫拦截）开关：启动时按配置初始化，面板可运行时切换。
	loopguard.SetEnabled(cfg.Features.LoopGuard)
	if !cfg.Features.LoopGuard {
		log.Printf("思考链死循环检测已关闭（features.loop_guard=false）")
	}
	_ = os.MkdirAll(cfg.AuthDir, 0o755)
	_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o755)

	// 多渠道运行时：每个渠道各自独立 pool + state + scheduler。
	stateDir := filepath.Dir(cfg.StateFile)
	// 旧版兼容：迁移 state.json → 分渠道 state-{kind}.json
	migrateStateFiles(cfg.StateFile, stateDir)

	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("读取 WorkBuddy 账号目录失败：%v", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 TraeWork 账号目录失败：%v", err)
	}
	qdAuths, err := auth.LoadQoderDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 Qoder 账号目录失败：%v", err)
	}
	wbaAuths, err := auth.LoadWorkBuddyAiDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 WorkBuddy 国际版账号目录失败：%v", err)
	}
	qwAuths, err := auth.LoadQwenWorkDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取千问办公账号目录失败：%v", err)
	}
	qcnAuths, err := auth.LoadQoderCNDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 QoderCN 账号目录失败：%v", err)
	}
	qcmAuths, err := auth.LoadQoderCOMDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 QoderCOM 账号目录失败：%v", err)
	}
	// 智谱清言：凭据是浏览器 Cookie 里的 chatglm_refresh_token（glm-*.json）。
	glmAuths, err := auth.LoadGLMDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取智谱清言账号目录失败：%v", err)
	}
	// MonkeyCode：同为「导入型」渠道，凭据由面板「从本机客户端导入」写入 auths/。
	mcAuths, err := auth.LoadMonkeyCodeDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 MonkeyCode 账号目录失败：%v", err)
	}
	// 小浣熊：导入型渠道，凭据由面板「从本机客户端导入」或浏览器授权登录写入 auths/。
	rcAuths, err := auth.LoadRaccoonDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取小浣熊账号目录失败：%v", err)
	}
	// Loomy：导入型渠道，凭据由面板「从本机客户端导入」写入 auths/。
	lmAuths, err := auth.LoadLoomyDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 Loomy 账号目录失败：%v", err)
	}
	// Cline：粘贴型渠道，凭据是官方 API Key（sk_…），由面板「粘贴 API Key」写入 auths/。
	// 一个 Cline 账号一把 Key；Key 不轮换 ⇒ 无 refresh、无保活、无签到。
	clAuths, err := auth.LoadClineDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 Cline 账号目录失败：%v", err)
	}
	// Comate：粘贴型渠道，凭据是 WPS 账号的 wps_sid Cookie，由面板「粘贴 wps_sid」写入 auths/。
	// Cookie 不轮换 ⇒ 无 refresh、无保活、无签到。
	cmAuths, err := auth.LoadComateDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 Comate 账号目录失败：%v", err)
	}
	// Lingxi：粘贴型渠道，凭据同为 WPS 账号的 wps_sid Cookie（灵犀侧门禁独立，须灵犀已开通），
	// 由面板「粘贴 wps_sid」写入 auths/lingxi-*.json。Cookie 不轮换 ⇒ 无 refresh/保活；
	// 但有每日签到（领智点），由调度器按 checkinMinutes 执行。
	lxAuths, err := auth.LoadLingxiDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 Lingxi 账号目录失败：%v", err)
	}
	// OfficeAce：导入型渠道，凭据由面板从本机 OfficeAce 客户端文件发现或自助 OAuth 登录写入 auths/。
	oaAuths, err := auth.LoadOfficeAceDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 OfficeAce 账号目录失败：%v", err)
	}
	// OpenCodeZen 匿名通道无凭证文件：全程只有一个虚拟账号，
	// 不经目录扫描、不参与 reload（见 app.reloadAccounts 的说明）。
	ocAuths := []*auth.Auth{oczen.AnonymousAuth()}
	// Kilo 免费池同样是免密渠道：没有凭证文件、没有登录流程，只有一个虚拟账号。
	kiAuths := []*auth.Auth{kilo.AnonymousAuth()}
	// MiniMax Code：正常多账号渠道，凭据由设备码登录写入 auths/minimax-*.json。
	mimAuths, err := auth.LoadMinimaxDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 MiniMax 账号目录失败：%v", err)
	}
	// LobsterAI：正常多账号渠道，凭据由本机回调登录写入 auths/lobsterai-*.json。
	lobAuths, err := auth.LoadLobsterAIDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 LobsterAI 账号目录失败：%v", err)
	}
	// ZCode：正常多账号渠道，凭据由服务端中介设备授权流登录写入 auths/zcode-*.json。
	zcdAuths, err := auth.LoadZCodeDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 ZCode 账号目录失败：%v", err)
	}
	// Gemini（Google Code Assist）：正常多账号渠道，凭据由 loopback OAuth 登录写入 auths/gemini-*.json。
	gemAuths, err := auth.LoadGeminiDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 Gemini 账号目录失败：%v", err)
	}
	// CodeArts（华为云 CodeArts Agent）：正常多账号渠道，凭据为 AK/SK + SecurityToken
	// （由 PKCE OAuth 登录写入 auths/codearts-*.json）。
	caAuths, err := auth.LoadCodeArtsDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("读取 CodeArts 账号目录失败：%v", err)
	}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d, qoder=%d, qodercn=%d, qodercom=%d, workbuddyai=%d, qwenwork=%d, glm=%d, monkeycode=%d, raccoon=%d, loomy=%d, cline=%d, comate=%d, lingxi=%d, officeace=%d, oczen=%d(匿名), kilo=%d(匿名), minimax=%d, lobsterai=%d, zcode=%d, gemini=%d, codearts=%d from %s",
		len(wbAuths), cfg.Region, len(trAuths), len(qdAuths), len(qcnAuths), len(qcmAuths), len(wbaAuths), len(qwAuths), len(glmAuths), len(mcAuths), len(rcAuths), len(lmAuths), len(clAuths), len(cmAuths), len(lxAuths), len(oaAuths), len(ocAuths), len(kiAuths), len(mimAuths), len(lobAuths), len(zcdAuths), len(gemAuths), len(caAuths), cfg.AuthDir)

	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}
	qdPool := pool.New(filepath.Join(stateDir, "state-qoder.json"))
	for _, a := range qdAuths {
		qoder.EnsureFingerprint(a) // 老凭证补机器指纹
		qdPool.Add(a)
	}
	wbaPool := pool.New(filepath.Join(stateDir, "state-workbuddyai.json"))
	for _, a := range wbaAuths {
		wbaPool.Add(a)
	}
	qwPool := pool.New(filepath.Join(stateDir, "state-qwenwork.json"))
	for _, a := range qwAuths {
		qwPool.Add(a)
	}
	qcnPool := pool.New(filepath.Join(stateDir, "state-qodercn.json"))
	for _, a := range qcnAuths {
		qodercn.EnsureFingerprint(a) // 老凭证补机器指纹
		qcnPool.Add(a)
	}
	qcmPool := pool.New(filepath.Join(stateDir, "state-qodercom.json"))
	for _, a := range qcmAuths {
		qodercom.EnsureFingerprint(a) // 老凭证补机器指纹
		qcmPool.Add(a)
	}
	// 智谱清言：与其它渠道同为多账号池（可加多个 refresh_token），
	// 但每个账号不可人工恢复（refresh_token 轮换后旧值即失效），
	// 故 Runtime 置 SingleAccount —— 见 runtimes 装配处的说明。
	glmPool := pool.New(filepath.Join(stateDir, "state-glm.json"))
	for _, a := range glmAuths {
		glmPool.Add(a)
	}
	mcPool := pool.New(filepath.Join(stateDir, "state-monkeycode.json"))
	for _, a := range mcAuths {
		mcPool.Add(a)
	}
	rcPool := pool.New(filepath.Join(stateDir, "state-raccoon.json"))
	for _, a := range rcAuths {
		rcPool.Add(a)
	}
	lmPool := pool.New(filepath.Join(stateDir, "state-loomy.json"))
	for _, a := range lmAuths {
		lmPool.Add(a)
	}
	// Cline：真多账号池（一把 Key = 一个 Cline 账号），故**不置 SingleAccount**，
	// 账号级惩罚与同请求换号（R29）全量适用。
	clPool := pool.New(filepath.Join(stateDir, "state-cline.json"))
	for _, a := range clAuths {
		clPool.Add(a)
	}
	// Comate：真多账号池（一份 wps_sid = 一个 WPS 账号），不置 SingleAccount。
	cmPool := pool.New(filepath.Join(stateDir, "state-comate.json"))
	for _, a := range cmAuths {
		cmPool.Add(a)
	}
	// Lingxi：真多账号池（一份 wps_sid = 一个 WPS 账号，灵犀侧开通），不置 SingleAccount。
	lxPool := pool.New(filepath.Join(stateDir, "state-lingxi.json"))
	for _, a := range lxAuths {
		lxPool.Add(a)
	}
	// OfficeAce：真多账号池（每个华为账号一份凭据），不置 SingleAccount。
	oaPool := pool.New(filepath.Join(stateDir, "state-officeace.json"))
	for _, a := range oaAuths {
		oaPool.Add(a)
	}
	// oczen：整池只有一个匿名虚拟账号，state 仅用于记冷却（无积分、无签到）
	ocPool := pool.New(filepath.Join(stateDir, "state-oczen.json"))
	for _, a := range ocAuths {
		ocPool.Add(a)
	}
	// 安全网：匿名账号不允许被禁用或被冷却（任何历史 state 或异常路径写脏都可自愈）。
	// 冷却也要清：该渠道只有这一个账号且无号可轮换，冷却即等于整条渠道下线。
	ocPool.SetDisabled(oczen.AnonymousUID, false)
	ocPool.ClearPenalty(oczen.AnonymousUID)
	// kilo：与 oczen 同款——整池只有一个虚拟账号，state 仅用于记冷却（无积分、无签到）。
	// 同样要在启动时自愈：任何历史脏数据（冷却/禁用）都会让这条没有第二个账号的渠道直接下线。
	kiPool := pool.New(filepath.Join(stateDir, "state-kilo.json"))
	for _, a := range kiAuths {
		kiPool.Add(a)
	}
	kiPool.SetDisabled(kilo.AnonymousUID, false)
	kiPool.ClearPenalty(kilo.AnonymousUID)
	// MiniMax：真多账号池（每个 MiniMax 账号一份 OAuth 凭据），不置 SingleAccount。
	mimPool := pool.New(filepath.Join(stateDir, "state-minimax.json"))
	for _, a := range mimAuths {
		mimPool.Add(a)
	}
	// LobsterAI：真多账号池（每个有道账号一份凭据，含 keyfrom 四元组），不置 SingleAccount。
	lobPool := pool.New(filepath.Join(stateDir, "state-lobsterai.json"))
	for _, a := range lobAuths {
		lobPool.Add(a)
	}
	// ZCode：真多账号池（每个智谱账号一份凭据 + device_mid），不置 SingleAccount。
	zcdPool := pool.New(filepath.Join(stateDir, "state-zcode.json"))
	for _, a := range zcdAuths {
		zcdPool.Add(a)
	}
	// Gemini：真多账号池（每个 Google 账号一份 OAuth 凭据 + project），不置 SingleAccount。
	gemPool := pool.New(filepath.Join(stateDir, "state-gemini.json"))
	for _, a := range gemAuths {
		gemPool.Add(a)
	}
	// CodeArts：真多账号池（每个华为账号一份 AK/SK + SecurityToken），不置 SingleAccount。
	caPool := pool.New(filepath.Join(stateDir, "state-codearts.json"))
	for _, a := range caAuths {
		caPool.Add(a)
	}

	wbUp := upstream.New()
	wbUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	trUp := traework.New()
	trUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// TraeCode（代码版）：与 TraeWork 同一上游、共用账号，仅 function 不同（solo_agent）。
	trCodeUp := traework.NewTraeCode()
	trCodeUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qdUp := qoder.New()
	qdUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	wbaUp := workbuddyai.New()
	wbaUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qwUp := qwenwork.New()
	qwUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qcnUp := qodercn.New()
	qcnUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qcmUp := qodercom.New()
	qcmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	mcUp := monkeycode.New()
	mcUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 等响应头上限由 NewWithTimeout 统一算（≥180s，R118）；总超时仍取 upstream.timeout_seconds。
	rcUp := raccoon.NewWithTimeout(time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second)
	lmUp := loomy.New()
	lmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	clUp := cline.New()
	// Cline：非流式 client 保留总超时（一问一答需要兜底）；StreamHTTP **不得**设 Timeout
	// ——R35：Client.Timeout 覆盖整个生命周期（含读 body），SSE 整个生成期都在读 body，
	// 长回答必被从中间掐断。共用 Transport 以复用连接池。
	clUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	clUp.StreamHTTP.Transport = clUp.HTTP.Transport
	cmUp := comate.New()
	// Comate：与 cline 同款——非流式 client 保留总超时；StreamHTTP 不设 Timeout（R35）。
	cmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	cmUp.StreamHTTP.Transport = cmUp.HTTP.Transport
	lxUp := lingxi.New()
	// Lingxi：与 comate 同款——非流式 client 保留总超时；StreamHTTP 不设 Timeout（R35）。
	lxUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	lxUp.StreamHTTP.Transport = lxUp.HTTP.Transport
	oaUp := officeace.New()
	// OfficeAce 上游为华为 AgentArts 网关：恒 stream=true，长回答（含思维链）常见，
	// 非流式 client 保留总超时兜底；StreamHTTP 不设 Timeout（R35）。
	oaUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	oaUp.StreamHTTP.Transport = oaUp.HTTP.Transport
	ocUp := oczen.New()
	// kilo：免密免费池。非流式 client 保留总超时兜底；StreamHTTP 不设 Timeout（R35）。
	// New() 内两个 client 已共用同一 Transport，这里无需再赋 Transport。
	kiUp := kilo.New()
	kiUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// MiniMax：推理是 Anthropic Messages 形状且恒流式，长思考常见 ⇒ 非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	mimUp := minimax.New()
	mimUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	mimUp.StreamHTTP.Transport = mimUp.HTTP.Transport
	// LobsterAI：对话是 OpenAI 兼容面且恒流式；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	lobUp := lobsterai.New()
	lobUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	lobUp.StreamHTTP.Transport = lobUp.HTTP.Transport
	// ZCode：Anthropic Messages 形态且恒流式；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	zcdUp := zcode.New()
	zcdUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	zcdUp.StreamHTTP.Transport = zcdUp.HTTP.Transport
	// Gemini：自有 v1internal 信封、恒流式；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	gemUp := gemini.New()
	gemUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	gemUp.StreamHTTP.Transport = gemUp.HTTP.Transport
	// CodeArts：对话走华为 snap-access（HMAC 签名）且长思考常见；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	caUp := codearts.New()
	// 不可用模型摘除记录落盘（R143）：81009.404/002002009 是账号权限结论、不会自愈，
	// 故持久化且无过期；重新登录成功后由 login 路径 ClearUnusable 清空。
	//
	// ⚠️ **绝不能**写进 state-codearts.json：那个文件由 pool.saveLocked 整文件覆写
	// （它不认识额外键），一次积分刷新/签到就会把摘除记录抹掉——线上实测 2026-10-26，
	// 表现为被摘除的模型重启后重新出现。两个写入者必须各用各的文件。
	caUp.UnusableFile = filepath.Join(stateDir, "models-codearts.json")
	caUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	caUp.StreamHTTP.Transport = caUp.HTTP.Transport
	glmUp := glm.New()
	// ⚠️ 只改 HTTP（非流式）的 Timeout。
	// **不要**给 glmUp.StreamHTTP 设 Timeout —— 流式请求一旦有整体超时，
	// 长回答会在超时点被掐断且无终止帧（实测 2026-09-27 触发 5 次）。
	glmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// 自定义渠道（R110）：与内建渠道同一口径——`upstream.timeout_seconds` 决定非流式
	// 总超时，同时把它当作「等响应头」上限的下限（大 prompt 首字节天然慢，不能用
	// 内建渠道那套紧值）。此处只算一次，下面两处 buildCustomRuntimes 共用。
	upstreamTimeout := time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// 开发期调试开关：把各渠道出站对话请求的**上游原始响应**落盘（仅 -tags devdump
	// 构建有效，见 internal/devdump）。这里只做一次启动期读取与提示；真正的接线在
	// 下方 wrapDumps（R57 起这是唯一统一改 Transport 的地方，且只在启动时做一次）。
	devdump.Refresh()
	if devdump.Enabled() {
		log.Printf("devdump enabled: 上游原始响应落盘至 %s（仅开发构建；含 token，勿外传）", os.Getenv("WILDWORK_DUMP_DIR"))
	}

	// 备用上游通道（全局单代理，**不参与常规请求**，仅渠道命中地区拒绝时自取）：
	// 只存地址，不碰任何渠道 client，故面板保存后无需热更新接线（见 transport.go 头注释）。
	applyBackupLink(cfg)

	// devdump 落盘接线表：必须覆盖**每一个**注册进 runtimes 的渠道 client。
	// 旧代理接线表就漏过 trCodeUp（两张表都漏）⇒ 同一个上游客户端变量多实例的渠道
	// （TraeCode 与 TraeWork 各一份 Client）最容易漏，由 transport_test.go 静态锁死。
	wrapDumps(map[string][]*http.Client{
		provider.WorkBuddy.String():   {wbUp.HTTP, wbUp.BillingHTTP, wbUp.StreamHTTP},
		provider.WorkBuddyAI.String(): {wbaUp.HTTP, wbaUp.StreamHTTP},
		provider.TraeWork.String():    {trUp.HTTP, trUp.StreamHTTP},
		provider.TraeCode.String():    {trCodeUp.HTTP, trCodeUp.StreamHTTP},
		provider.Qoder.String():       {qdUp.HTTP, qdUp.StreamHTTP},
		provider.QoderCN.String():     {qcnUp.HTTP, qcnUp.StreamHTTP},
		provider.QoderCOM.String():    {qcmUp.HTTP, qcmUp.StreamHTTP},
		provider.QwenWork.String():    {qwUp.HTTP, qwUp.StreamHTTP},
		provider.MonkeyCode.String():  {mcUp.HTTP, mcUp.StreamHTTP},
		provider.Raccoon.String():     {rcUp.HTTP, rcUp.StreamHTTP},
		provider.Loomy.String():       {lmUp.HTTP, lmUp.StreamHTTP},
		provider.Cline.String():       {clUp.HTTP, clUp.StreamHTTP},
		provider.Comate.String():      {cmUp.HTTP, cmUp.StreamHTTP},
		provider.Lingxi.String():      {lxUp.HTTP, lxUp.StreamHTTP},
		provider.OfficeAce.String():   {oaUp.HTTP, oaUp.StreamHTTP},
		provider.Oczen.String():       {ocUp.HTTP, ocUp.StreamHTTP},
		provider.Kilo.String():        {kiUp.HTTP, kiUp.StreamHTTP},
		provider.Minimax.String():     {mimUp.HTTP, mimUp.StreamHTTP},
		provider.LobsterAI.String():   {lobUp.HTTP, lobUp.StreamHTTP},
		provider.ZCode.String():       {zcdUp.HTTP, zcdUp.StreamHTTP},
		provider.Gemini.String():      {gemUp.HTTP, gemUp.StreamHTTP},
		provider.CodeArts.String():    {caUp.HTTP, caUp.StreamHTTP},
		provider.GLM.String():         {glmUp.HTTP, glmUp.StreamHTTP},
	})
	checkinMinutes, err := config.ParseClockTimes(cfg.Schedule.CheckinTimes)
	if err != nil {
		return nil, fmt.Errorf("解析签到时间失败：%v", err)
	}

	// Qoder 双区签到窗口（对齐上游 qoder2api 的调度机制）：
	// 每日 10:00（UTC+8）开放，活动可能在整点之后才创建，故 10:00–12:00 每分钟重试。
	const (
		qoderCheckinMinute     = 10 * 60 // 10:00
		qoderCheckinRetryUntil = 12 * 60 // 12:00（上游同款兑底截止）
	)

	// 积分流水记账器：在 scheduler 之前创建（签到后差分记账用）；
	// App 内部不重建，装配时统一注入。失败仅降级为无统计。
	lg, lgerr := ledger.New(filepath.Join(stateDir, "ledger"))
	if lgerr != nil {
		log.Printf("ledger init failed（统计功能不可用）: %v", lgerr)
	}

	wbSch := scheduler.New(scheduler.Config{Pool: wbPool, Upstream: wbUp, Name: "workbuddy", FollowGlobalCheckin: true, CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, Name: "traework", FollowGlobalCheckin: true, CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// Qoder 无签到活动：调度器只做 token keepalive（每日 refresh 保活）。
	// CheckinMinutes 用显式空切片关闭（传 nil 会被 scheduler.New 补成默认 9:00/21:00）。
	qdSch := scheduler.New(scheduler.Config{Pool: qdPool, Upstream: qdUp, Name: "qoder", CheckinMinutes: []int{}, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// WorkBuddy 国际版：无显式签到（ActivitiesOnly 模式）。
	// 定时仍对每个账号调用 DailyCheckin，其实现为「免费模型对话保活 + 签到探测」；
	// 不记录/上报签到状态，保持对用户透明。
	// Keepalive 关闭（token 有效期 365 天，无需每日刷新），用显式空切片关闭
	// （传 nil 会被 scheduler.New 补成默认 22:00）。
	wbaSch := scheduler.New(scheduler.Config{Pool: wbaPool, Upstream: wbaUp, Name: "workbuddyai", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, ActivitiesOnly: true, Ledger: lg})
	// 千问办公：无签到活动（每日积分服务端被动发放，无需保活/领取）；
	// CheckinMinutes + KeepaliveHours 均为显式空切片，本渠道没有任何定时任务
	// （token 由 deviceToken/refresh 按需轮换，定时保活反而会与千问办公 App
	// 互踩 —— 见备忘 §7.5 风险 1）。
	// 余额/费率靠 StartCreditAutoRefresh 循环拉取。
	qwSch := scheduler.New(scheduler.Config{Pool: qwPool, Upstream: qwUp, Name: "qwenwork",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, Ledger: lg})
	// QoderCN：仅 campaigns 活动路径（legacy daily-check-in 已 DISABLED，其 claim 恒返回
	// 409 会造成「假成功」，故不再使用）。签到窗口对齐上游 qoder2api：10:00 开放，
	// 活动可能在整点后才创建，故 10:00–12:00 之间每分钟重试，直到真正领到或超时。
	qcnSch := scheduler.New(scheduler.Config{Pool: qcnPool, Upstream: qcnUp, Name: "qodercn", CheckinMinutes: []int{qoderCheckinMinute}, CheckinRetryUntil: qoderCheckinRetryUntil, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// QoderCOM：仅 campaigns 活动路径（无 daily-check-in）；其余同 QoderCN。
	qcmSch := scheduler.New(scheduler.Config{Pool: qcmPool, Upstream: qcmUp, Name: "qodercom", CheckinMinutes: []int{qoderCheckinMinute}, CheckinRetryUntil: qoderCheckinRetryUntil, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// MonkeyCode：已实现签到（控制台 Cookie 走 POST {console}/api/v1/users/wallet/checkin，
	// 见 internal/monkeycode/checkin.go）。签到时刻用全局配置；**不设 CheckinRetryUntil**
	// ——它是「直接领一次」的接口，没有 Qoder 那种「活动整点后才创建」的延迟，
	// 每个签到时刻单次尝试即可（失败状态会在下一个时刻重试），避免窗口内每分钟空跑。
	// KeepaliveHours 仍为显式空切片：上游无 refresh 端点，保活没有可刷的东西。
	mcSch := scheduler.New(scheduler.Config{Pool: mcPool, Upstream: mcUp, Name: "monkeycode", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, Ledger: lg})
	// 小浣熊：签到走 setting_info（每日积分惰性落账的触发点，见 internal/raccoon/checkin.go）。
	// access_token 实测仅 ≈2h，而 refresh_token ≈30 天，
	// 故保活比其它渠道更密（每 4 小时一次）以减少「请求先 401 再刷新」的额外往返；
	// 请求路径上的 refreshIfSessionDead 仍会兜底自愈。
	//
	// 签到时间用全局配置（checkinMinutes）：上游按天去重、无活动上线延迟问题，
	// 故**不设 CheckinRetryUntil**（窗口重试是为「活动尚未创建」设计的，本渠道无此语义）。
	rcSch := scheduler.New(scheduler.Config{Pool: rcPool, Upstream: rcUp, Name: "raccoon", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{1, 5, 9, 13, 17, 21}, Ledger: lg})
	// Loomy：已实现签到（宠物每日打工奖励，POST /api/v1/pet-work/rewards，
	// 见 internal/loomy/checkin.go）——判据是该账号当日**有效对话轮次**（上游算），
	// 故未在官方客户端用过该账号时结果是「未达档位」（如实可重试状态）。
	// 签到时刻用全局配置、**不设 CheckinRetryUntil**：档位在一天里是渐进的，
	// 窗口内每分钟重试既无意义又违反 §0.1；每个签到时刻单次尝试即可。
	// KeepaliveHours 仍为显式空切片：**上游无 refresh 端点**（session 14 天，到期需重新登录/导入），
	// 保活没有可刷的东西（refreshToken 为空，调度器会跳过并只记一条日志）。
	lmSch := scheduler.New(scheduler.Config{Pool: lmPool, Upstream: lmUp, Name: "loomy", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, Ledger: lg})
	// Cline：官方 API Key **不过期**、无签到活动、无 refresh token ⇒ 定时任务全部关闭。
	// 必须传显式空切片 []int{}（R39）：传 nil 会被 scheduler.New 的零值兜底补上默认时间，
	// 于是每天空跑一次必然失败的签到，并在面板渲染红色「签到失败」。
	clSch := scheduler.New(scheduler.Config{Pool: clPool, Upstream: clUp, Name: "cline",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, Ledger: lg})
	// Comate：wps_sid Cookie **不过期**、无签到活动、无 refresh token ⇒ 定时任务全部关闭。
	// 必须传显式空切片 []int{}（R39）：传 nil 会被 scheduler.New 的零值兜底补上默认时间，
	// 于是每天空跑一次必然失败的签到，并在面板渲染红色「签到失败」。
	cmSch := scheduler.New(scheduler.Config{Pool: cmPool, Upstream: cmUp, Name: "comate",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, Ledger: lg})
	// Lingxi：有每日签到（daily_check_in 领智点，见 internal/lingxi/checkin.go），
	// 但 wps_sid Cookie 不过期、无 refresh token ⇒ 只开签到、不开保活。
	// 签到时刻用全局配置（上游按天去重、无活动上线延迟，不设 CheckinRetryUntil）。
	lxSch := scheduler.New(scheduler.Config{Pool: lxPool, Upstream: lxUp, Name: "lingxi", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, Ledger: lg})
	// OfficeAce：有每日签到（bonus/claim）但要先有临时凭据（V11 签名），保活用于续期。
	// CheckinMinutes 用全局签到时间；KeepaliveHours 用配置保活时段（临时凭据约 2h 到期）。
	oaSch := scheduler.New(scheduler.Config{Pool: oaPool, Upstream: oaUp, Name: "officeace", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// OpenCodeZen 匿名：无账号、无签到、无 token 可保活（凭证是常量 public）。
	// CheckinMinutes/KeepaliveHours 均为显式空切片 → 调度器不发生任何上游调用。
	ocSch := scheduler.New(scheduler.Config{Pool: ocPool, Upstream: ocUp, Name: "oczen",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	// Kilo 免费池：无账号、无签到、无 token 可保活 ⇒ 定时任务全关。
	// 必须传**显式空切片**（R39）：传 nil 会被 scheduler.New 当作「未配置」补成
	// 默认 9:00/21:00 签到与 22:00 保活，每天空跑两次必然失败的调用。
	kiSch := scheduler.New(scheduler.Config{Pool: kiPool, Upstream: kiUp, Name: "kilo",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	// MiniMax：有每日签到（7 天面板，claim 幂等判据是响应里的 claim_result），
	// 也需要 token 保活（refresh_token 会轮换，刷新后必须落盘）。
	mimSch := scheduler.New(scheduler.Config{Pool: mimPool, Upstream: mimUp, Name: "minimax", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// LobsterAI：有每日签到（三步：slot → context → check_in，客户端幂等键），
	// 也需要 token 保活（refresh 会轮换，刷新后必须落盘）。
	lobSch := scheduler.New(scheduler.Config{Pool: lobPool, Upstream: lobUp, Name: "lobsterai", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// ZCode：**无签到**（积分领取依赖阿里云验证码 + headful 浏览器，本仓不实现；
	// DailyCheckin 返回明确的「不支持」错误而不是伪造成功），且凭据**不可续期**
	// （JWT 无 exp）⇒ 定时任务全关。必须传显式空切片（R39）：传 nil 会被补成默认
	// 9:00/21:00 签到与 22:00 保活，每天空跑两次必然失败的调用并污染面板。
	zcdSch := scheduler.New(scheduler.Config{Pool: zcdPool, Upstream: zcdUp, Name: "zcode",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	// Gemini：**无签到**（DailyCheckin 返回明确的「不支持」错误），但有 refresh_token 可续期
	// ⇒ 只开保活；CheckinMinutes 用显式空切片关闭（R39：传 nil 会被补成默认 9:00/21:00，
	// 每天空跑两次必然失败的签到并污染面板）。
	gemSch := scheduler.New(scheduler.Config{Pool: gemPool, Upstream: gemUp, Name: "gemini",
		CheckinMinutes: []int{}, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// CodeArts：有每日签到（三步，走 HMAC 签名；本包实现了 CheckinReporter/CheckinGranter，
	// 签到结果按结构化状态上报），也需要 token 保活（续期后必须落盘，不变量 20）。
	caSch := scheduler.New(scheduler.Config{Pool: caPool, Upstream: caUp, Name: "codearts", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// 智谱清言：签到与对话联动（当日需先对话才能领打卡进度），故保活与签到
	// 合成一个流程（glm.Client.Keepalive 先发最小对话再签到）。
	// 用 DailyCheckin 通道驱动：CheckinMinutes 用全局签到时间，
	// KeepaliveHours 也用同一时段（保活=签到，不必额外一次）。
	glmSch := scheduler.New(scheduler.Config{Pool: glmPool, Upstream: glmUp, Name: "glm", FollowGlobalCheckin: true,
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: wbaPool, Upstream: wbaUp, StaticModels: workbuddyai.StaticModels(),
			// 国际版网关实测间歇性 502/503/504，账号本身健康，不计入账号错误
			NoCooldownOnServerError: true},
		provider.TraeWork: {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
		// TraeCode 与 TraeWork 共享 trPool（同一账号体系，避免 refresh token 轮换冲突），
		// 仅上游客户端不同（function=solo_agent）。
		provider.TraeCode:   {Kind: provider.TraeCode, Pool: trPool, Upstream: trCodeUp, StaticModels: server.TraeCodeStaticModels()},
		provider.Qoder:      {Kind: provider.Qoder, Pool: qdPool, Upstream: qdUp, StaticModels: qoder.StaticModels()},
		provider.QoderCN:    {Kind: provider.QoderCN, Pool: qcnPool, Upstream: qcnUp, StaticModels: qodercn.StaticModels()},
		provider.QoderCOM:   {Kind: provider.QoderCOM, Pool: qcmPool, Upstream: qcmUp, StaticModels: qodercom.StaticModels()},
		provider.QwenWork:   {Kind: provider.QwenWork, Pool: qwPool, Upstream: qwUp, StaticModels: qwenwork.StaticModels()},
		provider.MonkeyCode: {Kind: provider.MonkeyCode, Pool: mcPool, Upstream: mcUp, StaticModels: monkeycode.StaticModels()},
		provider.Raccoon:    {Kind: provider.Raccoon, Pool: rcPool, Upstream: rcUp, StaticModels: raccoon.StaticModels()},
		provider.Loomy:      {Kind: provider.Loomy, Pool: lmPool, Upstream: lmUp, StaticModels: loomy.StaticModels()},
		provider.GLM:        {Kind: provider.GLM, Pool: glmPool, Upstream: glmUp, StaticModels: glm.StaticModels()},
		provider.Cline:      {Kind: provider.Cline, Pool: clPool, Upstream: clUp, StaticModels: cline.StaticModels()},
		provider.Comate:     {Kind: provider.Comate, Pool: cmPool, Upstream: cmUp, StaticModels: comate.StaticModels()},
		provider.Lingxi:     {Kind: provider.Lingxi, Pool: lxPool, Upstream: lxUp, StaticModels: lingxi.StaticModels()},
		provider.OfficeAce:  {Kind: provider.OfficeAce, Pool: oaPool, Upstream: oaUp, StaticModels: officeace.StaticModels()},
		// 注意：GLM **不**置 SingleAccount。
		//
		// 早期误判：以为「凭据轮换后不可人工恢复」就该按单账号处理。但
		// ① 现在可用 CDP 自动登录随时补账号；② 单账号语义会**跳过所有账号级惩罚**，
		// 导致账号 A 失效/限流时不会切到账号 B —— 多账号完全失效（实测确认）。
		// 故按正常多账号渠道处理：错误分类惩罚 + 池内轮换。
		//
		// 唯一真实约束是「refresh_token 轮换必须落盘」，已由 glm.RefreshToken 保证。
		// oczen：整池只有一个匿名虚拟账号且不可重登——任何账号级惩罚（冷却/计数/禁用）
		// 都等于整条渠道下线。SingleAccount 声明该约束，handler 对所有错误一律原文透传；
		// NoCooldownOnServerError 保留（语义已被 SingleAccount 涵盖，留着不依赖顺序）。
		provider.Oczen: {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, StaticModels: oczen.StaticModels(),
			SingleAccount: true, NoCooldownOnServerError: true},
		// kilo：同为无账号渠道（免密免费池，虚拟账号不可重登），语义与 oczen 一致——
		// 任何账号级惩罚（冷却/计数/禁用）都等于整条渠道下线，故声明 SingleAccount，
		// 由 handler 对所有错误一律原文透传。
		provider.Kilo: {Kind: provider.Kilo, Pool: kiPool, Upstream: kiUp, StaticModels: kilo.StaticModels(),
			SingleAccount: true, NoCooldownOnServerError: true},
		// MiniMax：真多账号渠道 ⇒ 不置 SingleAccount（普通错误分类 + 池内轮换都适用）。
		provider.Minimax: {Kind: provider.Minimax, Pool: mimPool, Upstream: mimUp, StaticModels: minimax.StaticModels()},
		// LobsterAI：真多账号渠道 ⇒ 不置 SingleAccount（错误分类 + 池内轮换都适用）。
		provider.LobsterAI: {Kind: provider.LobsterAI, Pool: lobPool, Upstream: lobUp, StaticModels: lobsterai.StaticModels()},
		// ZCode：真多账号渠道 ⇒ 不置 SingleAccount（错误分类 + 池内轮换都适用）。
		provider.ZCode: {Kind: provider.ZCode, Pool: zcdPool, Upstream: zcdUp, StaticModels: zcode.StaticModels()},
		// Gemini：真多账号渠道 ⇒ 不置 SingleAccount。
		provider.Gemini: {Kind: provider.Gemini, Pool: gemPool, Upstream: gemUp, StaticModels: gemini.StaticModels()},
		// CodeArts：真多账号渠道 ⇒ 不置 SingleAccount。
		provider.CodeArts: {Kind: provider.CodeArts, Pool: caPool, Upstream: caUp, StaticModels: codearts.StaticModels(),
			ModelFilter: caUp.FilterUnusable},
	}
	appRuntimes := map[provider.Kind]*app.Runtime{
		provider.WorkBuddy:   {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, Scheduler: wbSch},
		provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: wbaPool, Upstream: wbaUp, Scheduler: wbaSch},
		provider.TraeWork:    {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, Scheduler: trSch},
		provider.TraeCode:    {Kind: provider.TraeCode, Pool: trPool, Upstream: trCodeUp, Alias: true}, // 别名渠道：无独立 Scheduler，账号聚合/签到/刷新均由 TraeWork 负责
		provider.Qoder:       {Kind: provider.Qoder, Pool: qdPool, Upstream: qdUp, Scheduler: qdSch},
		provider.QoderCN:     {Kind: provider.QoderCN, Pool: qcnPool, Upstream: qcnUp, Scheduler: qcnSch},
		provider.QoderCOM:    {Kind: provider.QoderCOM, Pool: qcmPool, Upstream: qcmUp, Scheduler: qcmSch},
		provider.QwenWork:    {Kind: provider.QwenWork, Pool: qwPool, Upstream: qwUp, Scheduler: qwSch},
		provider.MonkeyCode:  {Kind: provider.MonkeyCode, Pool: mcPool, Upstream: mcUp, Scheduler: mcSch},
		provider.Raccoon:     {Kind: provider.Raccoon, Pool: rcPool, Upstream: rcUp, Scheduler: rcSch},
		provider.Loomy:       {Kind: provider.Loomy, Pool: lmPool, Upstream: lmUp, Scheduler: lmSch},
		provider.GLM:         {Kind: provider.GLM, Pool: glmPool, Upstream: glmUp, Scheduler: glmSch},
		provider.Cline:       {Kind: provider.Cline, Pool: clPool, Upstream: clUp, Scheduler: clSch},
		provider.Comate:      {Kind: provider.Comate, Pool: cmPool, Upstream: cmUp, Scheduler: cmSch},
		provider.Lingxi:      {Kind: provider.Lingxi, Pool: lxPool, Upstream: lxUp, Scheduler: lxSch},
		provider.OfficeAce:   {Kind: provider.OfficeAce, Pool: oaPool, Upstream: oaUp, Scheduler: oaSch},
		provider.Oczen:       {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, Scheduler: ocSch},
		provider.Kilo:        {Kind: provider.Kilo, Pool: kiPool, Upstream: kiUp, Scheduler: kiSch},
		provider.Minimax:     {Kind: provider.Minimax, Pool: mimPool, Upstream: mimUp, Scheduler: mimSch},
		provider.LobsterAI:   {Kind: provider.LobsterAI, Pool: lobPool, Upstream: lobUp, Scheduler: lobSch},
		provider.ZCode:       {Kind: provider.ZCode, Pool: zcdPool, Upstream: zcdUp, Scheduler: zcdSch},
		provider.Gemini:      {Kind: provider.Gemini, Pool: gemPool, Upstream: gemUp, Scheduler: gemSch},
		provider.CodeArts:    {Kind: provider.CodeArts, Pool: caPool, Upstream: caUp, Scheduler: caSch},
	}

	initCustomServerRts, initCustomAppRts, customNames := buildCustomRuntimes(cfg.CustomChannels, stateDir, upstreamTimeout)
	for k, rt := range initCustomAppRts {
		appRuntimes[k] = rt
	}

	appInst, err := app.New(app.Options{
		ConfigPath: opts.ConfigPath,
		Config:     cfg,
		Runtimes:   appRuntimes,
		Ledger:     lg,
		Embedded:   opts.Embedded,
		OnQuit:     opts.OnQuit,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化失败：%v", err)
	}
	// 装配中途失败时释放 App 持有的文件/goroutine。成功路径由 Daemon.Stop 负责，
	// 这里用 ok 标记而不是「成功分支里关掉 defer」——defer 一旦注册就必然执行，
	// 只能靠标记让它变成空操作。
	started := false
	defer func() {
		if !started {
			appInst.Close()
		}
	}()

	// 调度器结果写日志（无 GUI 推送，日志即面板数据源）
	wbSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("workbuddy", r) })
	trSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("traework", r) })
	qdSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("qoder", r) })
	wbSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("workbuddy", uid, ok, msg) })
	trSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("traework", uid, ok, msg) })
	qdSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("qoder", uid, ok, msg) })
	wbaSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("workbuddyai", r) })
	wbaSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("workbuddyai", uid, ok, msg) })
	qwSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("qwenwork", uid, ok, msg) })
	glmSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("glm", r) })
	glmSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("glm", uid, ok, msg) })
	// 小浣熊签到已接入（setting_info 触发每日积分落账，见 internal/raccoon/checkin.go）：
	// 结果必须写日志——面板的「签到成功/失败」标签与 stats 的签到收入都读日志。
	rcSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("raccoon", r) })
	rcSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("raccoon", uid, ok, msg) })
	oaSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("officeace", r) })
	oaSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("officeace", uid, ok, msg) })

	// HTTP handler：OpenAI 端点 + Web UI + 管理 API
	inner := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		Ledger:       appInst.Ledger(),
		Stats:        appInst.Stats(), // 转发路径插桩 AddUsage/AddRequestRow（运行统计）
		HardCooldown: cfg.HardCreditDur,
		// 硬冷却截到每日发奖窗口（方案 B）：qwenwork 每日 00:00 UTC+8 发额度，
		// 账本实测 00:01~00:20 到账。固定 12h 会把 19:07 的 402 锁到次日 07:07，
		// 白锁 7 小时；按窗口截断后 00:30 即解冻，额度一到账就能用。
		HardCooldownFor:   cfg.HardCooldownFor,
		SoftCooldown:      cfg.SoftRateDur,
		ErrThreshold:      cfg.Cooldown.ErrThresh,
		ErrCooldown:       cfg.ErrCooldownDur,
		ModelFallbacks:    cfg.ModelFallback,                        // 启动时载入降级链（此前仅面板保存后热更新，重启即丢）
		ReasoningEfforts:  cfg.ReasoningEfforts,                     // 启动时载入思考强度映射（同上）
		MaxRequestBody:    int64(cfg.Server.MaxRequestBodyMB) << 20, // 0 = server 默认 128MiB
		StickyMaxRequests: cfg.Server.StickyMaxRequests,
		WebUI:             opts.WebFS,
		AttachAPI:         appInst.HandleAPI,
	})
	inner.SetCustomRuntimes(initCustomServerRts)

	// 两层结构：外层兼容层只接管三个新端点，其余（含 /v1/chat/completions、Web UI、
	// 管理 API）原样落到内层 handler，旧客户端的调用栈完全不变。
	// 渠道清单注入 Router，用于校验模型名前缀并给出可读报错。
	var builtinChannels []string
	for k := range runtimes {
		builtinChannels = append(builtinChannels, k.String())
	}

	// chMu/channels 持有「当前生效的完整渠道清单」（内建 + 启用中的自定义），供
	// compat syncer 与 custom syncer 共用（channelList 类型，便于单测锁住）。
	// 为什么必须是可变共享态而不能各自用启动快照：compat syncer 与 custom syncer 分属
	// 不同保存路径，若 compat syncer 闭包捕获启动快照，则「先加自定义渠道、
	// 后单改模型映射并保存」会把清单重置回启动态 ⇒ 新渠道被丢掉，/v1/responses 与
	// /v1/messages 报 unknown channel（R61 同族的「只实现一条通道」缺陷）。
	chList := &channelList{builtin: builtinChannels, custom: customNames}
	currentChannels := chList.snapshot

	compat := gateway.New(gateway.Config{
		Inner:  inner,
		APIKey: cfg.APIKey,
		Router: gateway.Router{
			Default:  cfg.Compat.DefaultChannel,
			Map:      cfg.Compat.ModelMap,
			Channels: gateway.SortChannels(currentChannels()),
			// 启动时也要带上别名表（R129）：不得只靠面板热更新——否则重启后
			// 「Chat 能用别名、Codex 不能用」，直到用户重新保存一次降级链。
			Aliases: config.AliasIndex(cfg.ModelFallback),
		},
		MaxTokensCap: cfg.Compat.MaxTokensCap,
	})
	// 面板保存 compat 时热更新兼容层路由表（不然新映射要重启才生效）
	appInst.SetCompatSyncer(func(defaultChannel string, maxTokensCap int, modelMap map[string]string) {
		compat.SetCompat(defaultChannel, maxTokensCap, modelMap, currentChannels())
	})
	// 面板保存模型降级链时热更新**两处**（R129）：内层 handler（降级路由 + 别名解析）
	// 与兼容层（Responses / Anthropic 的别名表）。旧实现只推内层，这是既有接线缺口——
	// 漏推的表现是「Chat 能用别名、Codex 不能」。兼容层的别名表走独立的 SetAliases，
	// 不扩展 SetCompat（后者由另一条保存路径负责，两条通道互不覆盖）。
	appInst.SetModelFallbackSyncer(func(rules []config.ModelFallbackRule) {
		inner.SetModelFallbacks(rules)
		compat.SetAliases(config.AliasIndex(rules))
	})
	// 面板保存思考强度映射时热更新内层 handler（不然新映射要重启才生效）
	appInst.SetReasoningEffortsSyncer(func(efforts map[string]string) {
		inner.SetReasoningEfforts(efforts)
	})
	// 面板保存自定义渠道时热更新内层 handler、app 运行时与兼容层路由表（R66）
	appInst.SetCustomChannelsSyncer(func(customChannels []custom.ChannelConfig) {
		newServerRts, newAppRts, newNames := buildCustomRuntimes(customChannels, stateDir, upstreamTimeout)
		inner.SetCustomRuntimes(newServerRts)
		appInst.UpdateCustomRuntimes(newAppRts)

		// 同步更新共享渠道清单（下一步 compat.SetCompat 用同一份，避免两条通道打架）
		chList.setCustom(newNames)

		compat.SetCompat(cfg.Compat.DefaultChannel, cfg.Compat.MaxTokensCap, cfg.Compat.ModelMap, currentChannels())
		inner.InvalidateModels()
	})
	// 备用上游通道无需热更新接线：面板保存走 app.SetBackupProxy → backuplink.Set（只换指针），
	// 渠道在命中地区拒绝时按需自取（R57），因此不存在旧的「两张接线表不一致」失效形态。
	// 面板保存 oczen key 后热更新渠道凭证；启动时也应用一次配置中的初始 key
	appInst.SetOczenSyncer(ocUp.SetAPIKey)
	ocUp.SetAPIKey(cfg.OczenAPIKey)
	compat.SetAPIKeySource(inner.CurrentAPIKey) // 面板改 API-Key 后，兼容层立即跟随
	compat.SetSystemOneHandler(func(w http.ResponseWriter, r *http.Request, state string, questions map[string]any) (int, []byte, error) {
		return ocUp.SystemOne(state, questions)
	})
	mux := http.NewServeMux()
	compat.Routes(mux) // POST /v1/responses · /v1/messages · /v1/messages/count_tokens · /v1/systemone
	mux.Handle("/", inner)
	if compat != nil {
		log.Printf("三接口兼容层已启用：default_channel=%q max_tokens_cap=%d model_map=%d 条",
			cfg.Compat.DefaultChannel, cfg.Compat.MaxTokensCap, len(cfg.Compat.ModelMap))
	}
	appInst.SetHandler(inner)
	appInst.SetRootHandler(mux)

	if err := appInst.StartServer(); err != nil {
		// 监听失败必须终止：只记日志继续跑会让调度器与托盘照常起来，
		// 表现为「面板看着一切正常、其实是别的实例在服务本端口」，
		// 而两个进程共享同一数据目录，会重复签到并互相覆盖刷新出的 token。
		//
		// 本包只**返回错误**（不自行退出）：桌面入口弹框后退出，Android 入口把错误
		// 回传成 Java 异常。判据保持不变——调用方必须终止进程，见 cmd 侧护栏测试。
		return nil, fmt.Errorf("监听 %s 失败：%v\n\n该端口可能已被占用（例如已有一个 wild-work 实例在运行）。\n"+
			"请先退出已有实例，或改用 config.json 的 listen.port（也可用环境变量 WILDWORK_LISTEN）。",
			cfg.Listen.Addr(), err)
	}

	// 调度器后台运行
	sctx, stop := context.WithCancel(context.Background())
	go wbSch.Run(sctx)
	go trSch.Run(sctx)
	go qdSch.Run(sctx)
	go wbaSch.Run(sctx)
	go qwSch.Run(sctx)
	go qcnSch.Run(sctx)
	go qcmSch.Run(sctx)
	go mcSch.Run(sctx)
	go rcSch.Run(sctx)
	go lmSch.Run(sctx)
	go ocSch.Run(sctx)
	// ⚠️ 新增渠道时必须在这里补 Run()。
	// 漏了**不会报错**——只表现为「该渠道的自动签到/保活从不运行」，
	// 而手工触发（面板按钮 / RunCheckinNow）仍然可用，极难发现。
	go glmSch.Run(sctx)
	// Cline 的 Run 同样不可漏（R34：漏了不报错，只表现为该渠道定时逻辑从不运行）。
	// 本渠道任务全关，Run 只会阻塞等 ctx/配置变更，但缺了它将来加任务即静默失效。
	go clSch.Run(sctx)
	// Comate 的 Run 同样不可漏（R34）：本渠道当前无任何定时任务，Run 只会阻塞等
	// ctx/配置变更；但漏了它，将来给它加任务就会静默不跑。
	go cmSch.Run(sctx)
	// Lingxi 的 Run 不可漏（R34）：本渠道有每日签到任务。
	go lxSch.Run(sctx)
	// OfficeAce 的 Run 不可漏（R34）：本渠道有签到与保活。
	go oaSch.Run(sctx)
	// kilo 的 Run 同样不可漏（R34）：本渠道当前无任何定时任务，Run 只会阻塞等
	// ctx/配置变更；但漏了它，将来给它加任务就会静默不跑。
	go kiSch.Run(sctx)
	// MiniMax 的 Run 不可漏（R34）：本渠道有签到与保活两条定时任务。
	go mimSch.Run(sctx)
	// LobsterAI 的 Run 不可漏（R34）：本渠道有签到与保活两条定时任务。
	go lobSch.Run(sctx)
	// ZCode 的 Run 同样不可漏（R34）：本渠道当前无定时任务，但漏了将来加任务即静默失效。
	go zcdSch.Run(sctx)
	// Gemini 的 Run 不可漏（R34）：本渠道有 token 保活任务。
	go gemSch.Run(sctx)
	// CodeArts 的 Run 不可漏（R34）：本渠道有签到与保活两条定时任务。
	go caSch.Run(sctx)

	// 积分自动刷新覆盖全部渠道：
	// - workbuddyai / qoder 无签到活动，不自动刷就会一直显示旧值或 0；
	// - traework / workbuddy(CN) 虽有签到顺带拉余额，但一天只有两次，
	//   其间 token 若在别处被轮换（401）也无法自愈；统一纳入循环才能
	//   启动即出真实拆分数字，并靠 401 自愈（refreshIfSessionDead）及时恢复。
	// oczen 与 kilo 不在其中：两条都是免密渠道（无账号、无积分可刷，面板显示「不适用」），
	// 其 FetchModelPricing 已在费率刷新路径单独覆盖。
	// glm 纳入：其凭据本体是 refresh_token（accessToken 可为空），
	// 积分靠 member_info 拉取，不刷就永远是 0。
	appInst.StartCreditAutoRefresh(sctx, []provider.Kind{
		// 旧 Qoder 渠道已下线：不自动刷积分/token（避免周期性 token refresh failed 噪音）
		provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork, provider.QoderCN, provider.QoderCOM, provider.QwenWork, provider.GLM,
		provider.MonkeyCode,
		provider.Raccoon,
		provider.Loomy,
		// MiniMax 有 credit/details 额度端点：不纳入就永远显示 0（面板看不到真实余额）。
		provider.Minimax,
		// LobsterAI 有 profile-summary 额度端点，同理必须纳入。
		provider.LobsterAI,
		// ZCode 的额度是 **token 口径**（credits_unit="token"）：纳入刷新是为了让面板能显示
		// Token 用量（用户 2026-10-17 的明确要求）；它的数字不进积分小计与积分流水
		// —— 由 app.creditsUnit 分流保证（积分流水跳过 token 渠道，条目一律 InfoOnly）。
		provider.ZCode,
		// Gemini **不**纳入：它的 UserResource 恒 0——真实信息是「配额窗口剩余百分比」，
		// 已按 InfoOnly 呈现在明细里、不参与积分算术，纳入只会刷出一个无意义的 0。
		// CodeArts 有 benefit 额度（按 UTC+8 自然日重置）与积分账户形态，必须纳入。
		provider.CodeArts,
		// Lingxi 有 credits/balance 余额端点（智点）：不纳入就只在每日签到时刷一次，
		// 对话消耗后面板数字最长滞后一个签到周期（2026-10-29 审查 W2 补登）。
		provider.Lingxi,
	}, app.CreditRefreshInterval)

	// 启动即刷新「模型列表 + 费率」，之后每 30 分钟。
	// 否则刚启动时费率缓存为空，面板下方全是 unknown（需手动点刷新才正常）。
	appInst.StartPricingAutoRefresh(sctx, app.PricingRefreshInterval)

	// Comate 空目录账号巡检（R135）：SubmitComateSID 只挡「新增」账号，
	// 而线上已有一批在 R135 之前加进去、账号侧一个模型都没有的 WPS 号。
	// 它们按 uid 顺序被选走 → mock 帧「未配置可用模型」→ 整条渠道对所有人失败，
	// 且该帧是 HTTP 200，handler 无法在同一请求内换号。
	// 故启动后异步扫一遍（零积分、一次 GET /models/账号），把这类号停用。
	go appInst.SweepComateEmptyAccounts()

	// 运行统计喂入器（账号状态差值/粘性路由/今日作废，30s 一轮）：
	// 进程内直调各数据源，面板「运行统计」tab 即时可用。
	appInst.StartStatsFeeders(sctx)

	started = true
	return &Daemon{
		Config: cfg,
		App:    appInst,
		Counts: map[string]int{
			"workbuddy": len(wbAuths), "traework": len(trAuths), "qoder": len(qdAuths),
			"oczen": len(ocAuths), "kilo": len(kiAuths), "minimax": len(mimAuths),
			"lobsterai": len(lobAuths), "zcode": len(zcdAuths), "gemini": len(gemAuths),
			"codearts": len(caAuths),
		},
		addr: appInst.ListenAddr(),
		stop: stop,
	}, nil
}

// migrateStateFiles 旧版 state.json 兼容：旧版 WorkBuddy 状态文件名为 state.json，
// 新版改为 state-workbuddy.json。若旧文件存在且新文件不存在，则重命名。
// TraeWork 状态文件始终是 state-traework.json，无需迁移。
func migrateStateFiles(oldStateFile, stateDir string) {
	newFp := filepath.Join(stateDir, "state-workbuddy.json")
	if _, err := os.Stat(newFp); err == nil {
		return // 新文件已存在
	}
	if _, err := os.Stat(oldStateFile); err != nil {
		return // 旧文件不存在
	}
	// 读旧文件确认是有效的 state JSON
	raw, err := os.ReadFile(oldStateFile)
	if err != nil {
		return
	}
	var sf struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		log.Printf("state.json 解析失败，跳过迁移: %v", err)
		return
	}
	if err := os.WriteFile(newFp, raw, 0o600); err != nil {
		log.Printf("迁移 state.json 失败: %v", err)
		return
	}
	_ = os.Rename(oldStateFile, oldStateFile+".old")
	log.Printf("已迁移 state.json → state-workbuddy.json")
}

// channelList 持有「当前生效的完整渠道清单」（内建 + 启用中的自定义），供 compat 与
// custom 两条保存通道共用。零值可用：snapshot 返回空表。
// 单测锁定「setCustom 后 snapshot 含新渠道、且不与内建重复」——这正是原始实现的缺陷：
// compat syncer 捕获启动快照，导致后加的自定义渠道在某条保存路径下被丢弃。
type channelList struct {
	mu      sync.Mutex
	builtin []string
	custom  []string
}

// setCustom 替换自定义渠道清单（内建部分不变）。
func (c *channelList) setCustom(custom []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.custom = append([]string(nil), custom...)
}

// snapshot 返回内建 + 自定义的完整渠道清单（拷贝，调用方可安全持有）。
func (c *channelList) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.builtin)+len(c.custom))
	out = append(out, c.builtin...)
	out = append(out, c.custom...)
	return out
}

// buildCustomRuntimes 将配置中的自定义渠道列表装配为 server 与 app 运行时（R66）。
//
// upstreamTimeout 是 `config.upstream.timeout_seconds`（0 = 渠道默认值）。内建渠道在
// Start 里逐个注入 `xxxUp.HTTP.Timeout`，自定义渠道原先漏了这条接线（R110）。
func buildCustomRuntimes(channels []custom.ChannelConfig, stateDir string, upstreamTimeout time.Duration) (map[provider.Kind]*server.Runtime, map[provider.Kind]*app.Runtime, []string) {
	sRuntimes := map[provider.Kind]*server.Runtime{}
	aRuntimes := map[provider.Kind]*app.Runtime{}
	var names []string

	for _, cc := range channels {
		k := provider.Kind(cc.Name)
		client := custom.NewWithTimeout(cc, upstreamTimeout)
		p := pool.New(filepath.Join(stateDir, "state-custom-"+cc.Name+".json"))
		au := &auth.Auth{
			Kind:      cc.Name,
			UID:       "custom-" + cc.Name,
			Nickname:  cc.Name + " (自定义)",
			ExpiresAt: 4102444800,
		}
		p.Add(au)
		p.SetDisabled(au.UID, false)
		p.ClearPenalty(au.UID)

		sRuntimes[k] = &server.Runtime{
			Kind:                    k,
			Pool:                    p,
			Upstream:                client,
			StaticModels:            client.StaticModels(),
			SingleAccount:           true,
			NoCooldownOnServerError: true,
		}
		aRuntimes[k] = &app.Runtime{
			Kind:     k,
			Pool:     p,
			Upstream: client,
			Custom:   true,
		}
		names = append(names, cc.Name)
		stats.RegisterChannelKind(cc.Name)
	}
	return sRuntimes, aRuntimes, names
}
