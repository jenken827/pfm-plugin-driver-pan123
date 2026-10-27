// 123 云盘驱动插件（官方插件）。
//
// 独立仓库分发，编译成一个独立进程，通过 loopback RPC 与 pfm 宿主通信。
// 安全模型见 pfm 仓库文档：插件与宿主同权限，只发布可信构建。
package main

import (
	_ "jenken827.com/jenken827/pfm-plugin-pan123/driver/pan123"
	"jenken827.com/jenken827/pfm/plugin/sdk"
)

func main() {
	sdk.MustServe("pan123")
}
