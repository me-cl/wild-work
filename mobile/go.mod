// 独立模块：只为 gomobile bind 服务，不参与桌面构建（根 go.mod 保持 go 1.25）。
//
// 为什么必须独立：gomobile 要求 `golang.org/x/mobile/bind` 出现在**当前模块**的
// 依赖图里，而 x/mobile 的最新版要求 go 1.26 —— 直接把依赖加进根 go.mod 会把整个
// 项目的语言版本抬到 1.26（CI 仍是 Go 1.25），为一条构建链路拖累全部平台。
// 放这里则升级隔离在本模块，桌面端 `go build ./...` 完全不受影响。
module wild-work/mobile

go 1.26.0

require wild-work v0.0.0

require (
	github.com/energye/systray v1.0.3 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	golang.org/x/image v0.47.0 // indirect
	golang.org/x/mobile v0.0.0-20261011034528-197770444867 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
)

replace wild-work => ../

tool golang.org/x/mobile/cmd/gobind
