#!/usr/bin/env bash
# build/package.sh —— 打包发版的统一入口。
#
# 存在的理由：打包链散在三处（AGENTS.md §4 / DEVELOPMENT.md §5 / shell/android/README.md），
# 每处都靠手抄命令，抄漏一步的症状又都很安静（没有图标、版本号没进二进制、
# Android 的 versionName 忘了跟着改）。把「手抄」变成「一条命令」，漏项才有地方兜。
#
# 完整说明见 docs/打包发版备忘.md。
#
# 用法：
#   build/package.sh check                 # 发版前校验（CI 不做，必须本地跑）
#   build/package.sh version               # 打印各处版本号，检查是否漂移
#   build/package.sh bump 2.11.6           # 同步 bump 版本号（Go 常量 + Gradle）
#   build/package.sh windows               # dist/wild-work.exe
#   build/package.sh linux [amd64|arm64|all]
#   build/package.sh macos [arm64|amd64]   # 只能在 macOS 上跑（cgo）
#   build/package.sh android [debug|release]
#   build/package.sh desktop               # windows + linux
#   build/package.sh all                   # desktop + android + macos（macOS 项自动跳过）
#
# 环境变量：
#   GRADLE      Android 用的 gradle（仓库不带 wrapper，默认找 PATH 里的 gradle）
#   ANDROID_HOME / ANDROID_NDK_HOME   Android SDK / NDK（NDK 可由 build-aar.sh 自动挑）
#   REQUIRE_DMG macOS 传 1 时 dmg 失败即致命（CI 就是这么用的）

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO_VERSION_FILE="internal/app/app.go"
GRADLE_FILE="shell/android/app/build.gradle"

log() { printf '==> %s\n' "$*"; }
warn() { printf '    !! %s\n' "$*" >&2; }
die() { printf '错误：%s\n' "$*" >&2; exit 1; }

# 帮助文本 = 文件头那段注释（第二条 `#` 行到第一条非注释行之前）。
# 不用写死行号：头注释增删一行帮助就截断或漏尾，是那种没人会发现的失效。
usage() {
  awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "$0"
}

# 版本号唯一真源：internal/app/app.go 的 `const Version = "x.y.z"`。
# macOS 的 build-app.sh 里有一份等价的 grep（CI 已验证多年，不去动它）；
# 改这里的取法时必须同步检查那一处。
project_version() {
  grep -oE 'const Version = "[^"]+"' "$GO_VERSION_FILE" | head -1 \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+'
}

gradle_version() {
  grep -oE 'versionName "[^"]+"' "$GRADLE_FILE" | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+'
}

gradle_version_code() {
  grep -oE 'versionCode [0-9]+' "$GRADLE_FILE" | head -1 | grep -oE '[0-9]+'
}

# 二进制里是否真的含这个版本号。strings 对 Go 二进制的长串不可靠，只能数原始字节。
verify_version_in_binary() {
  local file="$1" ver="$2" py=""
  if [ ! -f "$file" ]; then
    warn "产物不存在，跳过版本核对：$file"
    return 0
  fi
  if command -v python >/dev/null 2>&1; then
    py="python"
  elif command -v python3 >/dev/null 2>&1; then
    py="python3"
  fi
  if [ -z "$py" ]; then
    warn "未找到 python，跳过版本核对（请手工确认产物含 $ver）"
    return 0
  fi
  if ! "$py" - "$file" "$ver" <<'PY'
import sys

path, ver = sys.argv[1], sys.argv[2]
with open(path, "rb") as f:
    n = f.read().count(ver.encode())
print(f"    $ {path} 中含 {ver} 共 {n} 次")
sys.exit(0 if n >= 1 else 1)
PY
  then
    warn "版本号 $ver 未出现在 $file 中（构建可能没生效）"
  fi
}

# APK 里的版本号：直接数 libgojni.so 里的字节，不依赖 aapt2。
# 理由与 verify_version_in_binary 相同（Go 二进制里的长串只能用字节计数），
# 而且要证明的正是「Go 侧代码真的进了包」——aapt2 读的 versionName 只反映 Gradle，
# 两者都查才堵得住「Gradle 改了、AAR 忘重建」这种漂移。
verify_apk_version() {
  local apk="$1" ver="$2" py=""
  if [ ! -f "$apk" ]; then
    warn "产物不存在，跳过版本核对：$apk"
    return 0
  fi
  if command -v python >/dev/null 2>&1; then
    py="python"
  elif command -v python3 >/dev/null 2>&1; then
    py="python3"
  fi
  if [ -z "$py" ]; then
    warn "未找到 python，跳过 APK 版本核对"
    return 0
  fi
  if ! "$py" - "$apk" "$ver" <<'PY'
import sys, zipfile

apk, ver = sys.argv[1], sys.argv[2]
z = zipfile.ZipFile(apk)
seen = 0
bad = []
for i in z.infolist():
    if not i.filename.endswith("libgojni.so"):
        continue
    seen += 1
    n = z.read(i.filename).count(ver.encode())
    print(f"    $ {i.filename} 中含 {ver} 共 {n} 次")
    if n < 1:
        bad.append(i.filename)
sys.exit(0 if seen and not bad else 1)
PY
  then
    warn "APK 的原生库中未找到版本号 $ver（AAR 可能没重建）"
    return 1
  fi
}

# APK 体积体检：Gradle **增量**构建偶发产出「带零填充空洞」的包——对齐、签名、
# 版本号、.so 内容全部正常，只有体积异常（2026-11-17 实测 35.0MB -> 51.0MB，
# 多出的 16MB 是 lib/armeabi-v7a/libgojni.so 本地头之前一段全零，Gradle 只跑了
# 3 个 up-to-date 任务）。这种包能装能跑，只靠肉眼看大小才发现得了，故做成断言。
#
# 判据取 zip 的「未占用字节」= 中央目录起点 − 各条目本地头+数据之和：
# 正常包只有 zipalign 的页对齐填充（实测 27KB 级），异常包是兆级。
verify_apk_slack() {
  local apk="$1" limit="${APK_MAX_SLACK:-1048576}" py=""
  if command -v python >/dev/null 2>&1; then
    py="python"
  elif command -v python3 >/dev/null 2>&1; then
    py="python3"
  fi
  if [ -z "$py" ]; then
    warn "未找到 python，跳过 APK 体积体检"
    return 0
  fi
  "$py" - "$apk" "$limit" <<'PY'
import sys, zipfile

apk, limit = sys.argv[1], int(sys.argv[2])
z = zipfile.ZipFile(apk)
# 30 = 本地文件头的固定长度；文件名与 extra 的长度两边一致，够用。
occupied = sum(30 + len(i.filename.encode()) + len(i.extra) + i.compress_size
               for i in z.infolist())
slack = z.start_dir - occupied
print(f"    $ 未占用字节 {slack}（上限 {limit}）")
sys.exit(0 if slack <= limit else 1)
PY
}

# ------------------------------------------------------------------ check
cmd_check() {
  log "版本号一致性（Go 常量 vs Gradle）"
  cmd_version
  log "go build ./..."
  go build ./...
  log "go vet ./..."
  go vet ./...
  log "go test ./..."
  go test ./...
  if command -v node >/dev/null 2>&1; then
    log "node scripts/ui-static-check.mjs"
    node scripts/ui-static-check.mjs
  else
    warn "未找到 node，跳过前端静态检查（CI 只在 linux-amd64 跑这一项）"
  fi
  log "全部通过——CI 不做 go test / go vet，这一步不能省"
}

# ------------------------------------------------------------------ version
cmd_version() {
  local gover gver gcode
  gover="$(project_version)"
  gver="$(gradle_version)"
  gcode="$(gradle_version_code)"
  printf 'Go  const Version        %s\n' "$gover"
  printf 'Gradle versionName      %s\n' "$gver"
  printf 'Gradle versionCode      %s\n' "$gcode"
  if [ "$gover" != "$gver" ]; then
    warn "两处版本号不一致：Go=$gover Gradle=$gver"
    warn "Android 不是 CI 产物、没有编译期校验，发版前用 bump 一起改"
    return 1
  fi
  log "版本号一致：$gover"
}

# ------------------------------------------------------------------ bump
cmd_bump() {
  local new="${1:-}"
  [ -n "$new" ] || die "用法：build/package.sh bump <x.y.z>"
  case "$new" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) die "版本号必须是 x.y.z：$new" ;;
  esac
  case "$new" in
    *[!0-9.]*) die "版本号只能含数字和点：$new" ;;
  esac

  local old gcode newcode
  old="$(project_version)"
  gcode="$(gradle_version_code)"
  [ -n "$gcode" ] || die "读不到 $GRADLE_FILE 的 versionCode"
  newcode=$((gcode + 1))

  log "Go       $old -> $new"
  log "Gradle   $(gradle_version) -> $new（versionCode $gcode -> $newcode）"

  # 用临时文件 + mv 而不是 sed -i：BSD sed 的 -i 要额外参数，Git Bash 与 macOS 行为不同。
  local tmp
  tmp="$(mktemp)"
  sed "s/const Version = \"[^\"]*\"/const Version = \"$new\"/" "$GO_VERSION_FILE" > "$tmp"
  mv "$tmp" "$GO_VERSION_FILE"

  tmp="$(mktemp)"
  sed -e "s/versionName \"[^\"]*\"/versionName \"$new\"/" \
      -e "s/versionCode [0-9][0-9]*/versionCode $newcode/" \
      "$GRADLE_FILE" > "$tmp"
  mv "$tmp" "$GRADLE_FILE"

  if grep -qE "^# wild-work v${new}([^0-9.]|$)" RELEASES.md 2>/dev/null; then
    log "RELEASES.md 已有 v$new 段落"
  else
    warn "RELEASES.md 还没有 v$new 段落——CI 找不到段落会退化成 commit 列表，请追加"
  fi

  log "改完记得：go build ./... && build/package.sh check，然后重建 dist/wild-work.exe"
}

# ------------------------------------------------------------------ windows
cmd_windows() {
  local ver icon syso rsrc
  ver="$(project_version)"
  icon="cmd/wild-work/icon.ico"
  syso="cmd/wild-work/rsrc_windows_amd64.syso"
  mkdir -p dist

  # 图标与 .syso 都是生成文件、且在 .gitignore 里：新克隆的仓库必须现场生成。
  if [ ! -f "$icon" ]; then
    cp build/windows/icon.ico "$icon"
    log "已复制图标 build/windows/icon.ico -> $icon"
  fi
  if [ ! -f "$syso" ] || [ "$icon" -nt "$syso" ]; then
    rsrc=""
    if command -v rsrc >/dev/null 2>&1; then
      rsrc="$(command -v rsrc)"
    elif [ -x "$(go env GOPATH)/bin/rsrc" ]; then
      rsrc="$(go env GOPATH)/bin/rsrc"
    fi
    if [ -n "$rsrc" ]; then
      "$rsrc" -ico "$icon" -o "$syso"
      log "已生成 $syso"
    elif [ -f "$syso" ]; then
      warn "未找到 rsrc，沿用已有的 $syso"
    else
      warn "未找到 rsrc，本次产物将没有图标（go install github.com/akavel/rsrc@latest）"
    fi
  fi

  log "构建 dist/wild-work.exe（v$ver）"
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
    go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work
  verify_version_in_binary dist/wild-work.exe "$ver"
  log "产物：dist/wild-work.exe（$(wc -c < dist/wild-work.exe) B）"
}

# ------------------------------------------------------------------ linux
build_linux_arch() {
  local arch="$1" ver out
  ver="$(project_version)"
  out="dist/wild-work-linux-$arch"
  log "构建 $out（v$ver）"
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -o "$out" ./cmd/wild-work
  verify_version_in_binary "$out" "$ver"
}

cmd_linux() {
  local which="${1:-all}"
  mkdir -p dist
  case "$which" in
    amd64 | arm64) build_linux_arch "$which" ;;
    all) build_linux_arch amd64; build_linux_arch arm64 ;;
    *) die "未知架构：$which（可选 amd64 / arm64 / all）" ;;
  esac
  log "无桌面环境启动时必须带 --no-tray（否则托盘初始化 panic 退出）"
}

# ------------------------------------------------------------------ macos
cmd_macos() {
  local arch="${1:-$(uname -m)}" ver
  [ "$(uname -s)" = "Darwin" ] || die "macOS 产物只能在 macOS 上构建（cgo 必需），CI 里由 macos-latest 负责"
  case "$arch" in
    arm64 | amd64) ;;
    *) die "未知架构：$arch（可选 arm64 / amd64）" ;;
  esac
  ver="$(project_version)"
  mkdir -p dist
  log "构建 dist/wild-work-darwin-$arch（v$ver）"
  CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" \
    go build -trimpath -o "dist/wild-work-darwin-$arch" ./cmd/wild-work
  log "打包 .app + .dmg"
  ARCH="$arch" bash build/macos/build-app.sh
  log "产物：dist/WildWork.app / dist/WildWork-$ver-$arch.dmg"
}

# ------------------------------------------------------------------ android
cmd_android() {
  local kind="${1:-debug}" gradle task
  # 任务名不能用 sed 's/^./\U&/'：\U 是 GNU 扩展，macOS 的 BSD sed 不认
  # （会原样输出 "\Udebug"，Gradle 报 "Task '\Udebug' not found"）。
  case "$kind" in
    debug) task="assembleDebug" ;;
    release) task="assembleRelease" ;;
    *) die "未知类型：$kind（可选 debug / release）" ;;
  esac

  gradle="${GRADLE:-}"
  if [ -z "$gradle" ]; then
    gradle="$(command -v gradle || true)"
  fi
  [ -n "$gradle" ] || die "未找到 gradle（仓库不带 wrapper）：设 GRADLE=/path/to/gradle，需 8.7+，JAVA_HOME 指向 JDK 17"
  [ -n "${ANDROID_HOME:-}" ] || [ -f shell/android/local.properties ] \
    || warn "ANDROID_HOME 未设置且没有 local.properties，Gradle 可能找不到 SDK"

  if [ "$kind" = "release" ] && ! grep -q 'signingConfigs' "$GRADLE_FILE"; then
    warn "build.gradle 里还没有 signingConfigs，assembleRelease 产出的是未签名包"
  fi

  log "生成 AAR（gomobile bind，arm64 + arm）"
  bash shell/android/build-aar.sh

  log "Gradle $task"
  ( cd shell/android && "$gradle" "$task" )

  local apk="shell/android/app/build/outputs/apk/$kind/app-$kind.apk"
  [ -f "$apk" ] || die "没找到 APK：$apk"
  mkdir -p dist
  cp "$apk" "dist/wild-work-android-$kind.apk"
  log "产物：dist/wild-work-android-$kind.apk（$(wc -c < "dist/wild-work-android-$kind.apk") B）"

  # 体积体检不过 ⇒ 这是增量构建残留的零填充包，重跑一次 clean 就行。
  # 不自动 clean：clean 掉的中间产物会让下一次构建从零开始（gomobile bind 是分钟级），
  # 而绝大多数构建本来是干净的——由人决定值不值得付这个代价。
  if ! verify_apk_slack "dist/wild-work-android-$kind.apk"; then
    warn "APK 里有异常大的零填充（增量构建残留），请重跑："
    warn "  ( cd shell/android && \"\$GRADLE\" clean ) && build/package.sh android $kind"
    return 1
  fi
  verify_apk_version "dist/wild-work-android-$kind.apk" "$(project_version)"
}

# ------------------------------------------------------------------ dispatch
cmd_desktop() {
  cmd_windows
  cmd_linux all
}

cmd_all() {
  cmd_desktop
  cmd_android debug
  if [ "$(uname -s)" = "Darwin" ]; then
    cmd_macos "$(uname -m)"
  else
    warn "非 macOS 主机，跳过 macOS 产物（由 CI 或 macOS 真机构建）"
  fi
}

case "${1:-}" in
  check) cmd_check ;;
  version) cmd_version ;;
  bump) shift; cmd_bump "$@" ;;
  windows) cmd_windows ;;
  linux) shift; cmd_linux "$@" ;;
  macos) shift; cmd_macos "$@" ;;
  android) shift; cmd_android "$@" ;;
  desktop) cmd_desktop ;;
  all) cmd_all ;;
  "" | -h | --help | help) usage ;;
  *) usage; exit 2 ;;
esac
