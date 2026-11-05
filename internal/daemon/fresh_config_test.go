package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/config"
)

// 全新安装（没有 config.json）必须自动生成默认配置并继续启动。
//
// 这是一个**真实回归**的护栏：装配层从 main 抽到本包时，判断「文件不存在」
// 写成了 `os.IsNotExist(err)`，而 `config.Load` 返回的是
// `fmt.Errorf("read config: %w", err)`——`os.IsNotExist` **不做 unwrap**，
// 对包装错误恒返回 false ⇒ 首次启动被判成「配置损坏」直接拒绝。
// 桌面端因为用户已手动跑过、目录里早就有 config.json，问题被掩盖；Android 是
// 全新 filesDir，第一次启动必踩。
func TestFreshHomeConfigMissingIsRecognized(t *testing.T) {
	// ① 源码判据：装配层必须走 config.LoadOrCreate（它内部已用 errors.Is + fs.ErrNotExist）。
	// 这条同时守住两个坑：写成 os.IsNotExist（不 unwrap）会拒绝启动；手工
	// `config.Default()` 则会跳过 applyEnv（见 ③）。
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatalf("读 daemon.go 失败: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, "config.LoadOrCreate(") {
		t.Fatal("daemon.go 必须用 config.LoadOrCreate 加载配置：它既用 errors.Is 识别「文件不存在」，" +
			"又保证全新安装也会走 applyEnv（否则 WILDWORK_LISTEN 等环境变量在首次启动被静默丢弃）")
	}
	if strings.Contains(code, "os.IsNotExist(err)") {
		t.Fatal("daemon.go 不得用 os.IsNotExist 判断配置缺失（它不 unwrap，对包装错误恒为 false）")
	}
	if strings.Contains(code, "cfg = config.Default()") {
		t.Fatal("daemon.go 不得在「配置不存在」分支直接 config.Default()：那会跳过 applyEnv")
	}

	// ② 行为判据：守的是「config.Load 的错误能被 errors.Is 认出来」这个前提。
	// 若哪天 internal/config 把 %w 去掉，① 依然绿、② 先红——两者缺一不可。
	missing := filepath.Join(t.TempDir(), "config.json")
	_, loadErr := config.Load(missing)
	if loadErr == nil {
		t.Fatal("不存在的配置路径应当返回错误")
	}
	if !errors.Is(loadErr, fs.ErrNotExist) {
		t.Fatalf("errors.Is(err, fs.ErrNotExist) 必须为真（说明 config.Load 用了 %%w 包装），实际 err=%v", loadErr)
	}
	if os.IsNotExist(loadErr) {
		t.Skip("行为变了：os.IsNotExist 现在能识别包装错误，① 的注释需更新")
	}

	// ③ 行为判据：全新安装**必须吃环境变量**。
	// 现场：Android 外壳设 WILDWORK_LISTEN=127.0.0.1:0，首次启动时没有 config.json；
	// 早先装配层直接 config.Default() ⇒ applyEnv 被整条跳过 ⇒ 实际监听落到固定端口。
	t.Setenv("WILDWORK_LISTEN", "127.0.0.1:0")
	fresh := filepath.Join(t.TempDir(), "config.json")
	c, created, err := config.LoadOrCreate(fresh)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !created {
		t.Fatal("配置不存在时应报告 created=true")
	}
	if !c.Listen.Ephemeral || c.Listen.Addr() != "127.0.0.1:0" {
		t.Fatalf("全新安装必须应用 WILDWORK_LISTEN=127.0.0.1:0，实际 %+v addr=%s", c.Listen, c.Listen.Addr())
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("应把默认配置落盘：%v", err)
	}

	// ④ 已存在时行为必须与 Load 完全一致（含「加载不写盘」）。
	before, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := config.LoadOrCreate(fresh); err != nil || created {
		t.Fatalf("配置已存在时不得报告 created（err=%v created=%v）", err, created)
	}
	after, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("配置已存在时 LoadOrCreate 不得改写文件")
	}
}
