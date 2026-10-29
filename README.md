# pfm-plugin-driver-pan123

123 云盘存储驱动，[pfm](https://github.com/jenken827/pfm) 的官方插件（独立进程）。

部分方法的实现参考了 [OpenList](https://github.com/OpenListTeam/OpenList) 的 `drivers/123_open`；
没有 123 开放平台开发者凭据时，授权与续期走 **OpenList 公共授权服务**（<https://api.oplist.org/>）。

## 这是什么

[pfm](https://github.com/jenken827/pfm) 核心只内置 `local`；其余网盘以**独立进程插件**提供，通过 loopback RPC 与宿主通信。

⚠️ 插件与宿主**同权限**运行：能读宿主的 `config.yaml`（含全部 token）与 `data/`。
只安装官方发布或自己构建的二进制。

驱动走 123 云盘开放平台 Open API（`https://open-api.123pan.com`）：目录以 `fileId` 组织，
上传按「SHA1 秒传 → etag 秒传 → 分片上传 → 合并轮询」编排，下载用直链 + HTTP Range。

## 授权（没有开发者凭据也能用）

123 开放平台的正规玩法是在[开发者中心](https://www.123pan.com/developer)申请 `Client ID` +
`Client Secret`，再换取 `access_token`。但官方的 refresh_token 授权并未对个人开发者开放，
所以**没有这些凭据的用户可以直接用 OpenList 的公开授权系统：<https://api.oplist.org/>** ——
在页面上选 123云盘完成授权，把得到的 `refresh_token` 填进 [pfm](https://github.com/jenken827/pfm) 即可；
插件会把它当作 `refresh_ui` 令牌，向默认的续期服务 `https://api.oplist.org/123cloud/renewapi`
换 `access_token`，并在每次续期后**自动保存轮换出的新 `refresh_token`**，不需要手工更新。

认证按下面的优先级取用（`driver/pan123/token.go`）：

| 已填字段 | 行为 |
|---|---|
| `access_token` | 直接使用，按 **90 天**保守有效期（123 JWT 的 `exp` 与服务端判定口径不一致，故不采信 JWT）；过期或接口返回 401 后才走下面两条续期 |
| `refresh_token` + 续期服务地址 | 首次用填入值请求续期服务，之后一律用接口返回的**轮换后新值**；续期服务地址留空时回落到默认的 OpenList 地址 |
| `client_id` + `client_secret`（且没有 `refresh_token`） | `POST /api/v1/access_token` 换 token，按响应 `expiredAt`（RFC3339）计算有效期 |

续期失败会进入 **2 分钟冷却**，冷却期内不再请求续期接口（手上未过期的 token 仍可继续用）；
401 触发的强制续期之间至少间隔 60 秒，避免服务端持续 401 时把续期接口打爆。冷却结束自动解禁，
不需要重装插件。

从公共授权切到自有应用时，**先清空 `refresh_token`**：它优先于 `client_id`/`client_secret`，
留着就仍然走 OpenList 续期，而不是你自己的应用。

使用 OpenList 公共授权的最小配置：

| 字段 | 值 |
|---|---|
| Refresh Token | <https://api.oplist.org/> 上 123云盘授权后得到的令牌 |
| 续期服务地址 | 保持默认 `https://api.oplist.org/123cloud/renewapi` |
| Client ID / Client Secret / Access Token | **留空** |
| 根目录 FileId | 按需（默认 `0`，即网盘根目录） |

> 公共授权是第三方公共服务，可用性、频率限制与隐私边界由 OpenList 决定；介意的话就自己建应用。
> 插件只把 `refresh_token` 发给该续期地址，不发送其他配置。

## 配置项

| 字段 | 说明 | 默认 |
|---|---|---|
| `client_id` | 123 开放平台 Client ID，与 `client_secret` 搭配 | 空 |
| `client_secret` | 123 开放平台 Client Secret | 空 |
| `refresh_token` | OpenList 公开授权得到的令牌（续期接口的 `refresh_ui`） | 空 |
| `access_token` | 直接填写的 access_token，兜底用；优先级最高 | 空 |
| `api_url` | 续期服务地址；留空等价于默认值 | `https://api.oplist.org/123cloud/renewapi` |
| `root_file_id` | pfm 逻辑根目录对应的 123 `fileId`，`0` 表示网盘根 | `0` |
| `upload_thread` | 分片上传并发数（1-32） | `3` |

配置结构与表单字段声明在宿主侧 proto（`native.Pan123DriverConfig`）里，本仓库的
`driver/pan123/init.go` 只负责声明表单字段；JSON 键名与 `ConfigField.Name` 逐字一致，宿主把它当
不透明 JSON 字符串存取。加一个配置项要同时改 proto 与本仓库的字段声明。

凭据字段都可留空，但**至少要填一组**（`client_id` / `client_secret` 成对、`refresh_token`、`access_token` 任一）：
都没填时初始化只告警，实际请求时才报认证错误。
`refresh_token` 与 `client_id`/`client_secret` 同时存在时，续期优先用 `refresh_token`。

## 能力

- **上传**：SHA1 秒传 → etag（MD5）秒传 → 分片并发上传 → 合并轮询；同名文件自动重命名，不覆盖既有对象。123 不接受空文件。
- **目录**：物理目录按年/月/数字子目录自动分配创建，路径逐级解析为 `fileId` 并缓存；
  `mkdir` 拿不到 `fileId` 时回查确认，避免把下一级目录建到根目录下。
- **下载**：支持直链（`DirectLink`）与 HTTP Range 分片读取；服务端忽略 Range 时手动丢弃前置字节。
- **加密**：与 pfm 的内容加密互通，上传前加密、下载时解密。
- **删除**：按 `fileId` 批量移入回收站，单个 ID 非法不影响同批其他项。
- **只读列举**：支持 `RemoteLister`，可用于对账与孤儿文件清理。
- **限速**：按端点独立限速（1-5 QPS，`golang.org/x/time/rate`，不允许突发）；
  `code=429` 等 500ms 重试，`code=401` 续期后重试，整个请求最多尝试 3 次。

## 与 OpenList 的对应关系

`drivers/123_open` 是 123 云盘开放平台实现的参考，下面这些部分按其行为与参数重写（代码仍按 pfm 的驱动接口组织）：

| 部分 | 参考内容 |
|---|---|
| 端点、`platform: open_platform` 请求头、各端点 QPS 上限 | `drivers/123_open/util.go`、`meta.go` |
| 续期参数（`refresh_ui` / `server_use` / `driver_txt=123cloud_oa`）、`refresh_token` 轮换与回写 | `drivers/123_open/token.go` |
| 手填 `access_token` 的 90 天保守有效期 | `drivers/123_open/driver.go` |
| 上传编排：秒传、`create`、分片（`sliceNo` 从 1 开始、每片单独取 token）、`upload_complete` 轮询并忽略校验中状态 | `drivers/123_open/upload.go`、`driver.go` |
| 目录解析：`mkdir` 以错误为准、靠回查拿 `fileId`；合并期间把 `20103`（文件校验中）当中间状态继续轮询 | `drivers/123_open/util.go`、`driver.go` |
| `duplicate=2`（同名自动重命名） | `drivers/123_open/upload.go` |

OpenList 与 pfm 同为 AGPL-3.0。

## 构建

本仓库的 `go.mod` 通过 `replace` 直接依赖 [pfm](https://github.com/jenken827/pfm) 仓库的 Go 包
（驱动类型、proto、插件运行时），所以 pfm 源码要与本仓库放在**同一父目录**：

```bash
go build -trimpath -o pfm-plugin-driver-pan123 .
```

这些包在新版 pfm 里已拆到 [pfm-plugin-sdk](https://github.com/jenken827/pfm-plugin-sdk) 与
[pfm-proto](https://github.com/jenken827/pfm-proto)，本插件仍按旧路径导入，所以要用一份**仍包含这些包的 pfm 源码**。
宿主的驱动接口或 proto 变了就要跟着改并重编，宿主加载插件时也会比对 ABI 版本，不一致会直接报「协议版本不兼容」。

## 安装

目录布局（放到 [pfm](https://github.com/jenken827/pfm) 的 `plugin.dir` 下）：

```
plugins/pfm-plugin-driver-pan123/
  plugin.json
  pfm-plugin-driver-pan123
```

然后打开配置：

```yaml
plugin:
  enabled: true
```

发布进官方索引后可用 `pfm plugin install pfm-plugin-driver-pan123` 安装。

## 发布

```bash
scripts/make-release.sh --version 1.0.0 --seq 1 \
  --url-prefix https://<发布地址>/ --key <索引签名私钥>
```

先把 `plugin.json` 的 `version` 改成要发的版本（脚本与索引条目都从它取，别处没有第二份定义）。
产出 `release/pfm-plugin-driver-pan123-1.0.0-<platform>.tar.gz` 与签名索引 `index.json(.sig)`；
把包上传到 `--url-prefix` 指向的位置，索引与签名发布到索引仓库即可被
`pfm plugin install pfm-plugin-driver-pan123` 安装。私钥只在发布机上使用，不入仓库。

## 许可

与 [pfm 主仓库](https://github.com/jenken827/pfm) 一致（AGPL-3.0），部分实现参考
[OpenList](https://github.com/OpenListTeam/OpenList)（AGPL-3.0）。分发二进制时须同时提供对应源码。
