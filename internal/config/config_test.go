package config

import (
	"bytes"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen.Addr() != "127.0.0.1:17863" {
		t.Errorf("listen=%s", c.Listen.Addr())
	}
	if c.APIKey != "WildWorkAPI" {
		t.Errorf("api_key=%q", c.APIKey)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.HardCreditDur.Hours() != 12 {
		t.Errorf("hard=%v", c.HardCreditDur)
	}
	if c.Server.StickyMaxRequests != DefaultStickyMaxRequests {
		t.Errorf("server.sticky_max_requests=%d, want %d", c.Server.StickyMaxRequests, DefaultStickyMaxRequests)
	}
}

// 默认端口从 7863 改成 17863（2026-11-05，本机 7863 常被占用）。
// 两条判据缺一不可：① 没配端口的配置落新默认；② **显式写了 7863 的老配置原样沿用**——
// 悄悄改用户的监听地址，远比端口冲突难排查，也违反 R9「config 格式零迁移」。
func TestDefaultPortAppliesOnlyWhenUnset(t *testing.T) {
	t.Setenv("WILDWORK_API_KEY", "")

	// ① 缺 listen 段 ⇒ 落默认端口，且加载不写盘。
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	before := []byte(`{"api_key":"k","region":"cn"}`)
	if err := os.WriteFile(fp, before, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Listen.Addr(); got != "127.0.0.1:17863" {
		t.Fatalf("未配置端口应落默认 127.0.0.1:17863，实际 %s", got)
	}
	after, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("加载配置不得主动写盘：\nbefore=%s\nafter=%s", before, after)
	}

	// ② 显式 7863（老默认）⇒ 原样保留，不得被迁到 17863。
	fp2 := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(fp2, []byte(`{"listen":{"host":"127.0.0.1","port":7863}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Listen.Addr(); got != "127.0.0.1:7863" {
		t.Fatalf("显式配置的端口必须原样沿用，实际被改成了 %s", got)
	}

	// ③ 端口零值且**未要求随机端口** ⇒ 落默认。
	if got := (Listen{Host: "127.0.0.1"}).Addr(); got != "127.0.0.1:17863" {
		t.Fatalf("端口为零值应落默认 %d，实际 %s", DefaultPort, got)
	}
}

// 端口 0 是合法写法（内核分配），**不能**被当非法值静默丢弃。
//
// 现场：`WILDWORK_LISTEN=127.0.0.1:0` 曾整条失效——ParseListen 把 "0" 判成
// bad listen port，而 applyEnv 又只 `if err == nil` 才赋值 ⇒ 环境变量被吞。
// （Android 壳当前已改回固定端口，这条守的是显式 :0 这条能力本身。）
func TestParseListenAcceptsEphemeralPort(t *testing.T) {
	for _, in := range []string{"127.0.0.1:0", ":0", "0"} {
		l, err := ParseListen(in)
		if err != nil {
			t.Fatalf("ParseListen(%q) 应接受端口 0，实际报错 %v", in, err)
		}
		if !l.Ephemeral {
			t.Fatalf("ParseListen(%q) 必须标记 Ephemeral", in)
		}
		if got, want := l.Addr(), net.JoinHostPort(l.Host, "0"); got != want {
			t.Fatalf("ParseListen(%q).Addr()=%q，要求内核分配端口时必须保持 0（want %q）", in, got, want)
		}
	}

	// 非法端口仍要拒。
	for _, in := range []string{":-1", ":65536", ":notaport"} {
		if _, err := ParseListen(in); err == nil {
			t.Fatalf("ParseListen(%q) 应报错", in)
		}
	}

	// 配置文件里写 "port": 0 走的是 UnmarshalJSON 的对象分支，**不带** Ephemeral
	// 语义 ⇒ 仍回落默认端口（老行为不变，避免把用户写坏的 0 当随机端口）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"listen":{"host":"127.0.0.1","port":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Listen.Addr(); got != "127.0.0.1:17863" {
		t.Fatalf("配置文件里的 port:0 应回落默认，实际 %s", got)
	}
}

// 环境变量里的 :0 必须真的生效（此前被 applyEnv 静默吞掉）。
func TestEnvListenEphemeralSurvivesNormalize(t *testing.T) {
	t.Setenv("WILDWORK_LISTEN", "127.0.0.1:0")
	t.Setenv("WILDWORK_API_KEY", "")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Listen.Ephemeral {
		t.Fatalf("WILDWORK_LISTEN=127.0.0.1:0 未被应用：%+v", c.Listen)
	}
	if got := c.Listen.Addr(); got != "127.0.0.1:0" {
		t.Fatalf("normalize 不得把随机端口改回默认，实际 %s", got)
	}
}

func TestStickyMaxRequestsLegacyConfigDefaultsWithoutWriting(t *testing.T) {
	t.Setenv("WILDWORK_API_KEY", "")
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	before := []byte(`{"server":{"max_request_body_mb":128}}`)
	if err := os.WriteFile(fp, before, 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.StickyMaxRequests != DefaultStickyMaxRequests {
		t.Fatalf("缺字段应使用默认值 %d，实际 %d", DefaultStickyMaxRequests, c.Server.StickyMaxRequests)
	}
	after, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("加载配置不得主动写盘：\nbefore=%s\nafter=%s", before, after)
	}

	if err := Save(c, fp); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(saved, []byte(`"sticky_max_requests": 50`)) {
		t.Fatalf("用户保存配置后应写入默认值：%s", saved)
	}
}

func TestStickyMaxRequestsExplicitValue(t *testing.T) {
	t.Setenv("WILDWORK_API_KEY", "")
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{"server":{"sticky_max_requests":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.StickyMaxRequests != 7 {
		t.Fatalf("显式配置应为 7，实际 %d", c.Server.StickyMaxRequests)
	}
}

func TestStickyMaxRequestsInvalidFallsBackWithWarning(t *testing.T) {
	for _, value := range []int{0, -3} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Setenv("WILDWORK_API_KEY", "")
			dir := t.TempDir()
			fp := filepath.Join(dir, "config.json")
			raw := []byte(`{"server":{"sticky_max_requests":` + strconv.Itoa(value) + `}}`)
			if err := os.WriteFile(fp, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			var logs bytes.Buffer
			oldOutput, oldFlags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			log.SetFlags(0)
			defer func() {
				log.SetOutput(oldOutput)
				log.SetFlags(oldFlags)
			}()

			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if c.Server.StickyMaxRequests != DefaultStickyMaxRequests {
				t.Fatalf("非法值应回退为 %d，实际 %d", DefaultStickyMaxRequests, c.Server.StickyMaxRequests)
			}
			if got := logs.String(); !strings.Contains(got, "WARN config: server.sticky_max_requests=") {
				t.Fatalf("非法值应记录可检索的 warning，实际日志：%q", got)
			}
			after, err := os.ReadFile(fp)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, raw) {
				t.Fatalf("非法值只在内存回退，不得加载时主动写盘：\nbefore=%s\nafter=%s", raw, after)
			}
		})
	}
}

func TestExpiringDaysLegacyConfigDefaultsWithoutWriting(t *testing.T) {
	t.Setenv("WILDWORK_API_KEY", "")
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	// 老配置：R112 时代（无 expiring_days），且带着已被删除的 expiring_threshold_hours。
	before := []byte(`{"schedule":{"expiring_threshold_hours":24}}`)
	if err := os.WriteFile(fp, before, 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.ExpiringDays != DefaultExpiringDays {
		t.Fatalf("缺字段应使用默认值 %d，实际 %d", DefaultExpiringDays, c.Schedule.ExpiringDays)
	}
	after, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("加载配置不得主动写盘：\nbefore=%s\nafter=%s", before, after)
	}
	// 保存后才写出新字段（不变量 5：不因读一次配置就产生配置漂移）。
	if err := Save(c, fp); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(saved, []byte(`"expiring_days": 1`)) {
		t.Fatalf("用户保存配置后应写入默认值：%s", saved)
	}
}

func TestExpiringDaysExplicitValue(t *testing.T) {
	t.Setenv("WILDWORK_API_KEY", "")
	for _, days := range []int{1, 2, 7} {
		t.Run(strconv.Itoa(days), func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "config.json")
			raw := []byte(`{"schedule":{"expiring_days":` + strconv.Itoa(days) + `}}`)
			if err := os.WriteFile(fp, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if c.Schedule.ExpiringDays != days {
				t.Fatalf("显式配置应为 %d，实际 %d", days, c.Schedule.ExpiringDays)
			}
		})
	}
}

// TestExpiringDaysInvalidFallsBackWithWarning R112b：只应接受 1/2/7。
// 3 天是 R112 前的旧选项，现在必须回退默认并在日志里留下可检索痕迹——
// 否则用户手改 config.json 写个 3，面板会显示一个后端根本不用的窗口。
func TestExpiringDaysInvalidFallsBackWithWarning(t *testing.T) {
	for _, value := range []int{3, 5, -1, 999} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Setenv("WILDWORK_API_KEY", "")
			dir := t.TempDir()
			fp := filepath.Join(dir, "config.json")
			raw := []byte(`{"schedule":{"expiring_days":` + strconv.Itoa(value) + `}}`)
			if err := os.WriteFile(fp, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			var logs bytes.Buffer
			oldOutput, oldFlags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			log.SetFlags(0)
			defer func() {
				log.SetOutput(oldOutput)
				log.SetFlags(oldFlags)
			}()

			c, err := Load(fp)
			if err != nil {
					t.Fatal(err)
				}
			if c.Schedule.ExpiringDays != DefaultExpiringDays {
					t.Fatalf("非法值应回退为 %d，实际 %d", DefaultExpiringDays, c.Schedule.ExpiringDays)
				}
			if got := logs.String(); !strings.Contains(got, "WARN config: schedule.expiring_days=") {
					t.Fatalf("非法值应记录可检索的 warning，实际日志：%q", got)
				}
			after, err := os.ReadFile(fp)
			if err != nil {
					t.Fatal(err)
				}
			if !bytes.Equal(after, raw) {
					t.Fatalf("非法值只在内存回退，不得加载时主动写盘：\nbefore=%s\nafter=%s", raw, after)
				}
		})
	}
}

// TestIsExpiringDays 面板只允许 1/2/7（与前端 selExpiring 的三个 option 同集）。
func TestIsExpiringDays(t *testing.T) {
	for _, days := range []int{1, 2, 7} {
		if !IsExpiringDays(days) {
			t.Errorf("%d 应为合法临期口径", days)
		}
	}
	for _, days := range []int{0, 3, 5, 6, 8, -1} {
		if IsExpiringDays(days) {
			t.Errorf("%d 不应是合法临期口径", days)
		}
	}
}

func TestListenParsing(t *testing.T) {
	cases := []struct {
		in   string
		addr string
	}{
		{":9999", ":9999"},
		{"127.0.0.1:9999", "127.0.0.1:9999"},
		{"9999", ":9999"},
		{"0.0.0.0:7863", "0.0.0.0:7863"},
	}
	for _, c := range cases {
		l, err := ParseListen(c.in)
		if err != nil {
			t.Errorf("ParseListen(%q): %v", c.in, err)
			continue
		}
		if got := l.Addr(); got != c.addr {
			t.Errorf("ParseListen(%q).Addr()=%s want %s", c.in, got, c.addr)
		}
	}
	if _, err := ParseListen(":notaport"); err == nil {
		t.Error("want error for bad port")
	}
}

func TestLoadFile(t *testing.T) {
	t.Setenv("WILDWORK_API_KEY", "")
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":"127.0.0.1:9999","api_key":"k","region":"cn"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen.Addr() != "127.0.0.1:9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestLoadFileObjectListen(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":{"host":"192.168.1.2","port":8000}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen.Addr() != "192.168.1.2:8000" {
		t.Errorf("addr=%s", c.Listen.Addr())
	}
}

func TestSaveRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "sub", "c.json")
	c := Default()
	c.Listen.Host = "127.0.0.1"
	c.Schedule.CheckinHours = []int{8, 20}
	if err := Save(c, fp); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Listen.Addr() != "127.0.0.1:17863" {
		t.Errorf("addr=%s", c2.Listen.Addr())
	}
	if len(c2.Schedule.CheckinHours) != 2 || c2.Schedule.CheckinHours[0] != 8 {
		t.Errorf("hours=%v", c2.Schedule.CheckinHours)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WILDWORK_LISTEN", ":7777")
	t.Setenv("WILDWORK_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen.Addr() != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestClockTimes(t *testing.T) {
	got, err := ParseClockTimes([]string{"21:30", "09:05", "09:05"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 9*60+5 || got[1] != 21*60+30 {
		t.Fatalf("minutes=%v", got)
	}
	if _, err := ParseClockTimes([]string{"24:00"}); err == nil {
		t.Fatal("want invalid time error")
	}
	if got := FormatClockTimes(got); got[0] != "09:05" || got[1] != "21:30" {
		t.Fatalf("times=%v", got)
	}
}

func TestLoadLegacyCheckinHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[8,20]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Schedule.CheckinTimes; len(got) != 2 || got[0] != "08:00" || got[1] != "20:00" {
		t.Fatalf("times=%v", got)
	}
}

func TestLoadMinuteCheckinTimes(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_times":["09:05","21:30"]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Schedule.CheckinTimes; len(got) != 2 || got[0] != "09:05" || got[1] != "21:30" {
		t.Fatalf("times=%v", got)
	}
}

func TestAdminPasswordAndLoopback(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	// 旧配置（无 admin_password）必须能加载，且默认空 = 面板不鉴权
	os.WriteFile(fp, []byte(`{"listen":"127.0.0.1:9999"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminPass != "" {
		t.Errorf("缺省 admin_password 应为空，实际 %q", c.AdminPass)
	}
	// 新字段可读
	os.WriteFile(fp, []byte(`{"listen":{"host":"0.0.0.0","port":8000},"admin_password":"pw12345678"}`), 0o600)
	if c, err = Load(fp); err != nil {
		t.Fatal(err)
	} else if c.AdminPass != "pw12345678" {
		t.Errorf("admin_password=%q", c.AdminPass)
	}
	// env 覆盖
	t.Setenv("WILDWORK_ADMIN_PASSWORD", "env-pw")
	if c, err = Load(fp); err != nil || c.AdminPass != "env-pw" {
		t.Errorf("env override 失败：%v %q", err, c.AdminPass)
	}
	// IsLoopback
	for host, want := range map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true, "0.0.0.0": false, "": false, "192.168.1.9": false} {
		if got := (Listen{Host: host}).IsLoopback(); got != want {
			t.Errorf("IsLoopback(%q)=%v want %v", host, got, want)
		}
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}
