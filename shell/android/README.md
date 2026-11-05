# Android 外壳（Route B：gomobile bind + WebView）

把同一个 `wild-work` 装进手机：Go 侧全套逻辑（多渠道聚合、轮换、签到、账本）编译成
AAR 在**进程内**跑，外面只有一个 WebView 指向 `http://127.0.0.1:<port>/` 加载面板。
面板本身**一行未改**——它就是桌面端 embed 的那份 `internal/webui/web`。

```
┌────────────────────── APK ──────────────────────┐
│ MainActivity (Kotlin)  ──WebView──▶ 127.0.0.1:N │
│        │                                        │
│        └─广播─▶ DaemonService (前台服务)         │
│                        │                        │
│                        └──▶ libgojni.so         │
│                              internal/daemon    │
│                              （与桌面产物同源）  │
└─────────────────────────────────────────────────┘
```

## 为什么是这个方案

| 方案 | 结论 |
|------|------|
| **B：gomobile bind + Kotlin WebView 壳** | 采用。原生 app，前台服务保活，面板复用 |
| A：Termux 里跑命令行 | 可行但用户要手敲命令、没有图标、没有保活 |
| C：把二进制塞进 jniLibs 自己 fork exec | 能跑，但拿不到进程内生命周期、要自己管 pid/回收 |

## 构建

### 1. 生成 AAR

```bash
export ANDROID_HOME="$HOME/Android/Sdk"
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/27.3.13750724"
GOPROXY=https://goproxy.cn,direct shell/android/build-aar.sh
```

脚本做的事：`mobile/`（嵌套模块）里 `gomobile bind -target=android/arm64,android/arm
-androidapi 26 -javapkg com.wildwork`，产物拷进 `shell/android/app/libs/wildwork.aar`。

> **为什么有 `mobile/go.mod`**：gomobile 要求 `golang.org/x/mobile/bind` 出现在
> **当前模块**的依赖图里，而 x/mobile 最新版要求 go 1.26。把它加进根 `go.mod` 会把
> 整个项目的语言版本抬上去（为一条构建链路拖累全部平台，CI 还在 1.25）。嵌套模块
> （`replace wild-work => ../`）把版本升级隔离住了。

### 2. 编 APK

```bash
cd shell/android
./gradlew assembleDebug          # 或本机已装的 gradle
# 产物 app/build/outputs/apk/debug/app-debug.apk
adb install -r app/build/outputs/apk/debug/app-debug.apk
# 本机验收时顺手拷一份到 dist/（dist/ 不入库，只当本地产物目录用）
cp app/build/outputs/apk/debug/app-debug.apk ../../dist/wild-work-android-debug.apk
```

`local.properties` 里的 `sdk.dir=` 由本机生成、**不入库**（见 `.gitignore`）。
没有 wrapper 时直接用本机 gradle（需 8.7+；`JAVA_HOME` 指向 JDK 17）。

### 3. release 签名（可选）

```bash
keytool -genkeypair -v -keystore ww.jks -keyalg RSA -keysize 2048 -validity 10000 -alias wildwork
# 然后在 app/build.gradle 的 signingConfigs 里指过去
./gradlew assembleRelease
```

## 运行行为

- 首次启动：`POST_NOTIFICATIONS` 授权（Android 13+）→ 起前台服务 → 服务在
  `Context.filesDir` 下建 `config.json`/`auths/`/`data/` → 面板自动加载。
- 端口：**固定**，壳不传任何监听环境变量，由 `config.json` 决定——
  全新安装是 `config.DefaultPort`（**17863**），用户在面板「设置 → 监听」里改过就
  沿用改后的值。重启不变，`adb forward`、把面板地址记进手机上的客户端才成立。
  （2026-11-05 修正：原先壳设 `WILDWORK_LISTEN=127.0.0.1:0` 让内核随机分配，
  结果是**每次重启面板地址都变**；WebView 本来就按 `Mobile.url()` 取地址，随机端口
  换不来任何好处。）
  两条必须保持的点：① 壳**不得**设置 `WILDWORK_LISTEN`（任何值）——`applyEnv` 的
  优先级高于 `config.json`，写死会把用户在面板里改的地址永久压掉，设置项看着还在
  却重启即失效（`internal/daemon/mobile_fixed_port_test.go` 静态锁死）；
  ② 全新安装必须走 `config.LoadOrCreate`（**不能** `config.Default()`），否则
  `applyEnv` 被整条跳过——它不只管端口，还管 `WILDWORK_AUTH_DIR` /
  `WILDWORK_STATE_FILE` 等数据目录。
  `Listen.Ephemeral` / `ParseListen` 接受端口 `0` 的能力保留，服务的是
  `WILDWORK_LISTEN=<host>:0` 这类显式请求，**与 Android 壳无关**。
- 端口被占用时**不会自选下一个**：`daemon.Start` 返回错误、前台服务报「启动失败」
  （R68）。手机上一次启动两个实例才会撞，正常只有一个前台服务。
- 保活：前台服务 + `START_STICKY`。被系统回收后重建时 `onCreate` 会重新 `Mobile.start`
  （幂等，状态全在 `filesDir` 里）。
- 停止：通知栏的「停止」，或系统设置里强制停止；面板里的「退出」也能停——
  `app.Options.Embedded` 让它在嵌入式宿主里**只停服务、不 `os.Exit`**（进程是宿主
  App 的，杀掉连带灭掉界面，而且 `START_STICKY` 会立刻把它拉回来），
  `DaemonService` 轮询 `Mobile.running()`，发现已停就 `stopSelf()` 摘掉通知。
  轮询必须等**装配成功之后**才挂：装配期间 `running()` 恒 false，挂早了会把正在
  启动的服务误判成「用户已退出」。
- **手机作为网关**：同一局域网的其他设备把 API 基址指到
  `http://<手机局域网 IP>:<port>/v1` 即可。但这要求监听 `0.0.0.0` 并设面板密码
  （R4/§0.2：非环回监听强制要求密码），当前壳固定 `127.0.0.1`，未开放该路径。

## 已知限制

| 限制 | 说明 |
|------|------|
| 仅 arm64/arm | `gomobile bind` 只编这两种 ABI；x86 模拟器跑不起来 |
| 无托盘 | Android 没有系统托盘；`internal/systray` 是 `//go:build !android` 的桩 |
| 无自动更新 | 桌面端靠 `dist/` 与 CI 产物更新，手机端需重装 APK |
| 登录流程 | 各渠道的 CDP 自动登录（`internal/cdp`）在手机上拉不起浏览器，**必须用「粘贴凭据 / 扫码」或「打开网页」+ 手工回调的路径**（R131/R132/R137 已覆盖 comate 扫码、WPS 灵犀扫码、手工粘贴兜底）。面板不复用桌面端的「自动打开浏览器」：后端下发 `embedded: true`（`app.Options.Embedded`），前端据此**一律不自动打开**，只给「打开网页」（经 JS 桥 `WildWorkHost.openExternal` 转交系统浏览器）与「复制链接」两个按钮；raccoon/lingxi 改道粘贴/客户端导入，glm 直接展开手工粘贴，codearts 用「打开网页」+ 粘贴回环回调地址 |
| 后台限制 | 国产 ROM（MIUI/EMUI/ColorOS）会额外限制前台服务，需在系统设置里给「忽略电池优化」 |
| 明文 HTTP | 面板走 `http://127.0.0.1`，已用 `network_security_config.xml` 只对环回开白名单 |

## 文件结构

```
shell/android/
├── build-aar.sh                     # gomobile bind → app/libs/wildwork.aar
├── settings.gradle / build.gradle    # AGP 8.7.3 + Kotlin 2.0.21
├── gradle.properties
└── app/
    ├── build.gradle                 # minSdk 26 / targetSdk 35 / abiFilters arm*
    ├── proguard-rules.pro           # 不混淆 gomobile 生成的类（JNI 按名字绑定）
    └── src/main/
        ├── AndroidManifest.xml      # 前台服务 + 通知权限 + 环回明文白名单
        ├── java/com/wildwork/app/
        │   ├── MainActivity.kt      # WebView 壳 + 广播接收 + 返回键历史
        │   └── DaemonService.kt     # 前台服务，持 Mobile.start/stop
        └── res/
            ├── mipmap-*/ic_launcher.png
            ├── values/{strings,themes}.xml
            └── xml/network_security_config.xml
```

Go 侧对应入口只有 `mobile/mobile.go`（导出 `start`/`stop`/`url`/`err`/`running`/
`version`/`waitReady`/`status`），装配逻辑与桌面端共用 `internal/daemon`。
