#!/usr/bin/env bash
# 构建指定平台的插件发布包（plugin.json + 可执行文件），并打印 sha256。
#
# 用法：
#   scripts/package.sh --version 1.0.0 --goos linux --goarch amd64 [--out dist]
#
# 产物：
#   <out>/<plugin.json 的 name>-<version>-<goos>-<goarch>.tar.gz
#
# 包名、可执行文件名都取自 plugin.json，所以 CI（release.yml）与手工发布
# （make-release.sh）用的是同一份定义，不会两边漂移。
# 交叉编译不需要 CGO，任何平台都能构建其它平台的包。
set -euo pipefail

# sha256 兼容（macOS 没有 sha256sum）
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

version=""
goos=""
goarch=""
out="dist"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --goos) goos="$2"; shift 2 ;;
    --goarch) goarch="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done
[[ -n "$version" && -n "$goos" && -n "$goarch" ]] || {
  echo "用法: $0 --version <版本> --goos <GOOS> --goarch <GOARCH> [--out <目录>]" >&2
  exit 1
}

cd "$(dirname "$0")/.."

# 唯一来源：plugin.json
manifest() { python3 -c 'import json,sys; print(json.load(open("plugin.json"))[sys.argv[1]])' "$1"; }
name=$(manifest name)
bin=$(basename "$(manifest exec)")
[[ "$goos" == "windows" ]] && bin="${bin}.exe"
platform="${goos}-${goarch}"
pkg="${name}-${version}-${platform}"

mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
  go build -trimpath -ldflags "-s -w" -o "$work/$bin" .
cp plugin.json "$work/"
# 包根只有 plugin.json 与可执行文件：宿主解包后按 plugin.json 的 exec 直接启动
tar -czf "$out/${pkg}.tar.gz" -C "$work" plugin.json "$bin"

sha=$(sha256_of "$out/${pkg}.tar.gz")
echo "包: $out/${pkg}.tar.gz"
echo "平台: ${platform}"
echo "sha256: ${sha}"
