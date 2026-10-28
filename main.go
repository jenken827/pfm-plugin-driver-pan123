// 123 云盘驱动插件（官方插件）。
//
// 独立仓库分发，编译成一个独立进程，通过 loopback RPC 与 pfm 宿主通信。
// 插件与宿主同权限运行（同一用户、同一文件系统），因此只应发布可信构建。
package main

import (
	_ "jenken827.com/jenken827/pfm-plugin-driver-pan123/driver/pan123"
	"jenken827.com/jenken827/pfm/plugin/sdk"
)

func main() {
	sdk.MustServe("pan123")
}
