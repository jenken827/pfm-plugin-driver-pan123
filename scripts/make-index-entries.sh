#!/usr/bin/env bash
# 根据已构建的发布包生成索引条目（未签名），供合并进索引仓库。
#
# 用法：
#   scripts/make-index-entries.sh --version 1.0.0 --tag v1.0.0 \
#     --repo owner/name [--dir dist] [--base-url https://github.com/owner/name/releases/download]
#
# 输出：JSON 数组（直接贴进索引的 plugins 里，再由索引仓库签名）
#
# 条目字段（kind / name / <kind>_id / abi_version）都取自 plugin.json，本脚本不重复声明。
# 只在索引仓库里合并与签名：整份索引共用一个单调递增的 seq，各插件仓库各签一份
# 会互相覆盖。
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
tag=""
repo=""
dir="dist"
base_url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --tag) tag="$2"; shift 2 ;;
    --repo) repo="$2"; shift 2 ;;
    --dir) dir="$2"; shift 2 ;;
    --base-url) base_url="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done
[[ -n "$version" && -n "$tag" && -n "$repo" ]] || {
  echo "用法: $0 --version <版本> --tag <tag> --repo <owner/name> [--dir <目录>] [--base-url <url>]" >&2
  exit 1
}
[[ -d "$dir" ]] || { echo "目录不存在: $dir" >&2; exit 1; }

cd "$(dirname "$0")/.."
manifest() { python3 -c 'import json,sys; print(json.load(open("plugin.json"))[sys.argv[1]])' "$1"; }
kind=$(manifest kind)
name=$(manifest name)
id=$(manifest "${kind}_id")
abi=$(manifest abi_version)

# 默认按 GitHub Release 资产的地址拼，与 package.sh 的文件名一致
base_url="${base_url:-https://github.com/${repo}/releases/download}"
prefix="${name}-${version}-"
released_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)

entries=""
for pkg in "$dir"/${prefix}*.tar.gz; do
  [[ -e "$pkg" ]] || { echo "没有找到 ${prefix}*.tar.gz" >&2; exit 1; }
  file=$(basename "$pkg")
  # 文件名尾部就是平台：<name>-<version>-<goos>-<goarch>.tar.gz
  platform="${file#"$prefix"}"
  platform="${platform%.tar.gz}"
  sha=$(sha256_of "$pkg")
  entry=$(cat <<JSON
    {
      "kind": "${kind}",
      "name": "${name}",
      "${kind}_id": "${id}",
      "version": "${version}",
      "abi_version": ${abi},
      "platform": "${platform}",
      "url": "${base_url}/${tag}/${file}",
      "sha256": "${sha}",
      "min_pfm_version": "",
      "released_at": "${released_at}"
    }
JSON
)
  if [[ -n "$entries" ]]; then
    entries="${entries},
${entry}"
  else
    entries="${entry}"
  fi
done

printf '[\n%s\n]\n' "$entries"
