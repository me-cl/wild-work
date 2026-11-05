#!/usr/bin/env bash
# 生成 gomobile 的 AAR（Android 外壳依赖它）。
#
# 为什么用独立模块：仓库根的 go.mod 是 go 1.25，而 golang.org/x/mobile 最新版要求
# go 1.26。把它加进根模块会把整个项目的语言版本抬上去（CI 的 setup-go 仍是 1.25），
# 为一条构建链路拖累全部平台。`mobile/` 是嵌套模块（`replace wild-work => ../`），
# 语言版本升级隔离在这里，桌面端 `go build ./...` 完全不受影响。
#
# 前置：
#   1. Go 1.26+（只有这个脚本需要；桌面构建仍是 1.25）
#   2. Android SDK + NDK（ANDROID_HOME / ANDROID_NDK_HOME）
#   3. `go install golang.org/x/mobile/cmd/gomobile@latest` + `gomobile init`
#
# 用法：
#   shell/android/build-aar.sh            # 生成并拷进 app/libs
#   shell/android/build-aar.sh --no-copy  # 只生成到 build/android
#
# 国内网络需要模块代理：
#   GOPROXY=https://goproxy.cn,direct shell/android/build-aar.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/build/android"
AAR="$OUT/wildwork.aar"
LIBS="$ROOT/shell/android/app/libs"

# gomobile 要求 golang.org/x/mobile/bind 在当前模块的依赖图里，
# 且必须在 `mobile/` 目录下执行（它不是根模块）。
if ! grep -q 'golang.org/x/mobile' "$ROOT/mobile/go.mod" 2>/dev/null; then
  echo "==> 补依赖 golang.org/x/mobile（mobile/ 模块内）" >&2
  (cd "$ROOT/mobile" && go get -tool golang.org/x/mobile/cmd/gobind@latest)
fi

if ! command -v gomobile >/dev/null 2>&1; then
  GOMOBILE="$(go env GOPATH)/bin/gomobile"
  [ -x "$GOMOBILE" ] || { echo "未找到 gomobile，请先 go install golang.org/x/mobile/cmd/gomobile@latest" >&2; exit 1; }
else
  GOMOBILE="$(command -v gomobile)"
fi

# NDK 版本：显式指定，避免 gomobile 自己挑到 .partial / .broken 目录。
#
# 目录名形如 `27.3.13750724`（**本身就带点**），所以不能用 `grep -v '\.'` 去筛——
# 那会把全部有效版本滤掉，NDK 变量变成空串，gomobile 随即失败（2026-11-05 踩到）。
# 正确判据是「整串只由数字和点组成」：SDK 里的 `27.3.13750724.broken2` /
# `...partial` 带非数字后缀，一律排除。
if [ -z "${ANDROID_NDK_HOME:-}" ] && [ -n "${ANDROID_HOME:-}" ] && [ -d "$ANDROID_HOME/ndk" ]; then
  newest=""
  for d in "$ANDROID_HOME"/ndk/*/; do
    [ -d "$d" ] || continue
    v="$(basename "$d")"
    case "$v" in *[!0-9.]*) continue ;; esac   # 带非数字后缀（.broken2/.partial）⇒ 跳过
    if [ -z "$newest" ] || [ "$(printf '%s\n%s\n' "$newest" "$v" | sort -V | tail -1)" = "$v" ]; then
      newest="$v"
    fi
  done
  if [ -n "$newest" ]; then
    ANDROID_NDK_HOME="$ANDROID_HOME/ndk/$newest"
    export ANDROID_NDK_HOME
    echo "==> NDK: $newest" >&2
  fi
fi

mkdir -p "$OUT"
echo "==> gomobile bind（arm64 + arm，androidapi 26）" >&2
cd "$ROOT/mobile"
"$GOMOBILE" bind \
  -target=android/arm64,android/arm \
  -androidapi 26 \
  -javapkg com.wildwork \
  -o "$AAR" .

echo "==> 产出 $AAR（$(du -h "$AAR" | cut -f1)）" >&2

if [ "${1:-}" != "--no-copy" ]; then
  mkdir -p "$LIBS"
  cp "$AAR" "$LIBS/"
  echo "==> 已复制到 $LIBS/wildwork.aar" >&2
fi
