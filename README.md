# pfm-pan123

123 云盘存储驱动，pfm 的官方插件（独立进程）。

## 这是什么

pfm 核心只内置 `local`；其余网盘以**独立进程插件**提供，通过 loopback RPC 与宿主通信
（协议与安全模型见 pfm 仓库 `DESIGN.md` §11.4）。

⚠️ 插件与宿主**同权限**运行：能读宿主的 `config.yaml`（含全部 token）与 `data/`。
只安装官方发布或自己构建的二进制。

## 构建

```bash
# 需要 pfm 源码在同一父目录（go.mod 里用 replace 指向 ../pfm）
go build -o pfm-plugin-baidu .
```

## 安装

目录布局（放到 pfm 的 `plugin.dir` 下）：

```
plugins/pfm-pan123/
  plugin.json
  pfm-plugin-baidu
```

然后打开配置：

```yaml
plugin:
  enabled: true
```

或从官方索引安装：`pfm plugin install pfm-pan123`。

## 发布

```bash
scripts/make-release.sh --version 1.0.0 --seq 1 \
  --url-prefix https://<发布地址>/ --key <索引签名私钥>
```

产出 `release/pfm-pan123-1.0.0-<platform>.tar.gz` 与签名索引 `index.json(.sig)`；
把包上传到 `--url-prefix` 指向的位置，索引与签名发布到索引仓库即可被
`pfm plugin install pfm-pan123` 安装。私钥只在发布机上使用，不入仓库。

## 许可

与 pfm 主仓库一致（AGPL-3.0）。分发二进制时须同时提供对应源码。
