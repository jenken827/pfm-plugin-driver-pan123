module github.com/jenken827/pfm-plugin-driver-pan123

go 1.26.0

require (
	github.com/jenken827/pfm-plugin-sdk v0.3.0
	golang.org/x/time v0.16.0
)

require (
	github.com/sirupsen/logrus v1.9.4 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// 本地要连改 SDK 时用工作区，不要往这里加 replace：
//   go work init . ../pfm-plugin-sdk   （go.work 不入库）
