#!/usr/bin/env bash
# 手工发布（没有 CI 时用）：构建本机平台的插件包 → 生成索引 → 签名。
#
# 用法：
#   scripts/make-release.sh --version 1.0.0 --seq 1 --repo <owner>/<name> \
#     --base-url https://example.com/plugins --key <索引签名私钥>
#
# 产物（release/）：
#   <plugin.json 的 name>-<version>-<platform>.tar.gz   插件包（plugin.json + 可执行文件）
#   index.json / index.json.sig                         签名索引（含本次这一条）
#
# 条目里的 url = <base-url>/v<version>/<文件名>，与 CI 用 Release 资产拼出来的形状一致。
# 私钥只在发布机上使用，不要放进仓库。日常发布走 CI（推送 tag 触发 release.yml）。
set -euo pipefail

version=""
seq=""
repo=""
base_url=""
key=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --seq) seq="$2"; shift 2 ;;
    --repo) repo="$2"; shift 2 ;;
    --base-url) base_url="$2"; shift 2 ;;
    --key) key="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done
[[ -n "$version" && -n "$seq" && -n "$repo" && -n "$base_url" && -n "$key" ]] || {
  echo "用法: $0 --version <版本> --seq <n> --repo <owner>/<name> --base-url <url> --key <私钥文件>" >&2
  exit 1
}

cd "$(dirname "$0")/.."

goos=$(go env GOOS)
goarch=$(go env GOARCH)
bash scripts/package.sh --version "$version" --goos "$goos" --goarch "$goarch" --out release

entries=$(bash scripts/make-index-entries.sh \
  --version "$version" --tag "v${version}" --repo "$repo" \
  --dir release --base-url "$base_url")

python3 - "$seq" "$entries" <<'PY' > release/index.json
import json, sys
seq = int(sys.argv[1])
plugins = json.loads(sys.argv[2])
print(json.dumps({"seq": seq, "plugins": plugins}, indent=2, ensure_ascii=False))
PY

pfm plugin sign-index --key "$key" release/index.json
echo "索引: release/index.json（seq=${seq}）"
