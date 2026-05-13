module github.com/NimoTech/NimoOS-Wiki

go 1.21

require (
	github.com/NimoTech/NimoOS-Common v0.0.0-00010101000000-000000000000
	github.com/coreos/go-systemd v0.0.0-20191104093116-d3cd4ed1dbcf
	github.com/fsnotify/fsnotify v1.7.0
	github.com/labstack/echo/v4 v4.12.0
	github.com/labstack/echo-jwt/v4 v4.2.0
	github.com/mattn/go-sqlite3 v1.14.42
	github.com/spf13/viper v1.18.0
	github.com/stretchr/testify v1.9.0
	go.uber.org/zap v1.27.1
)

replace github.com/NimoTech/NimoOS-Common => ../NimoOS-Common
