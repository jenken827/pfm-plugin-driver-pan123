#!/usr/bin/env bash
# 构建本机平台的插件发布包，并生成可直接发布的签名索引。
#
# 用法：
#   scripts/make-release.sh --version 1.0.0 --seq 1 \
#     --url-prefix https://example.com/plugins/ --key /path/to/index.key
#
# 产物（release/）：
#   pfm-plugin-driver-pan123-<version>-<platform>.tar.gz   插件包（plugin.json + 可执行文件）
#   index.json / index.json.sig             签名索引（含本次这一条）
#
# 私钥只在发布机上使用，不要放进仓库。
set -euo pipefail

version=""
seq=""
url_prefix=""
key=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --seq) seq="$2"; shift 2 ;;
    --url-prefix) url_prefix="$2"; shift 2 ;;
    --key) key="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done
[[ -n "$version" && -n "$seq" && -n "$url_prefix" && -n "$key" ]] || {
  echo "用法: $0 --version <v> --seq <n> --url-prefix <url> --key <私钥文件>" >&2; exit 1
}

cd "$(dirname "$0")/.."
goos=$(go env GOOS)
goarch=$(go env GOARCH)
platform="${goos}-${goarch}"
name="pfm-plugin-driver-pan123-${version}-${platform}"

go build -o "release/pfm-plugin-driver-pan123" .
# 用临时目录组织包结构：plugin.json 与可执行文件都在包根
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp plugin.json "release/pfm-plugin-driver-pan123" "$work/"
tar -czf "release/${name}.tar.gz" -C "$work" plugin.json pfm-plugin-driver-pan123
sha=$(sha256sum "release/${name}.tar.gz" | cut -d' ' -f1)

cat > release/index.json <<JSON
{
  "seq": ${seq},
  "plugins": [
    {
      "kind": "driver",
      "name": "pfm-plugin-driver-pan123",
      "driver_id": "pan123",
      "version": "${version}",
      "abi_version": 1,
      "platform": "${platform}",
      "url": "${url_prefix}${name}.tar.gz",
      "sha256": "${sha}",
      "min_pfm_version": "",
      "released_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    }
  ]
}
JSON

pfm plugin sign-index --key "$key" release/index.json
echo "包: release/${name}.tar.gz"
echo "sha256: ${sha}"
echo "索引: release/index.json（seq=${seq}）"
