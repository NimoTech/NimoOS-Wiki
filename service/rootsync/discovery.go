package rootsync

import (
	"os"
	"strings"
)

// fallbackBaseURL 是服务发现文件缺失或为空时使用的兜底地址。
// 核心服务默认监听在本机 80 端口(见 Gateway 反代惯例),不放行 localhost 校验也走此地址。
const fallbackBaseURL = "http://127.0.0.1"

// resolveBaseURL 每次请求前都重新读取一次服务发现文件解析出核心的 base URL。
// 之所以不缓存,是因为核心重启后监听端口可能变化(见 nimoos.url 由核心启动时写入),
// 每次请求读文件能保证始终打到当前存活的核心实例;文件缺失/读取失败/内容为空
// 一律回退到 fallbackBaseURL,不让发现失败阻塞调用方(失败由上层置 needsReconcile 兜底)。
func resolveBaseURL(discoveryFile string) string {
	b, err := os.ReadFile(discoveryFile)
	if err != nil {
		return fallbackBaseURL
	}
	base := strings.TrimSpace(string(b))
	if base == "" {
		return fallbackBaseURL
	}
	return base
}
