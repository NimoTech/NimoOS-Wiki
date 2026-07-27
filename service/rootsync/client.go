// Package rootsync 是 NimoOS-Wiki 向 NimoOS 核心推送 root 授权的 HTTP client。
//
// 授权源解耦背景:核心是唯一的授权权威(o_root_grants 表),Wiki 侧对 root 目录的
// 增/删/启停操作需要把结果推给核心,核心据此决定 agent 能访问哪些目录。
// 推送是尽力而为(best-effort):任何一次调用失败,均由调用方(Task 5 的 manager)
// 将对应 root 标记为 needsReconcile,并在 Wiki 服务启动时做一次全量 Reconcile 兜底,
// 因此本包不做重试/退避,只负责把单次 HTTP 调用的结果(2xx/非 2xx/网络错误)如实
// 转换为 nil/error。
package rootsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// requestTimeout 是单次推送请求的超时时间。核心是内网服务,3s 足够;
// 超时也会被当作失败处理,交由调用方置 needsReconcile。
const requestTimeout = 3 * time.Second

// Grant 是推给核心的单条 root 授权记录,JSON tag 需与核心
// /v1/nimoos/_internal/root-grants 系列端点的请求体字段精确对齐。
type Grant struct {
	RootID  string `json:"root_id"`
	Path    string `json:"path"`
	Enabled bool   `json:"enabled"`
}

// Client 是核心授权推送客户端。
type Client struct {
	discoveryFile string
	httpClient    *http.Client
}

// New 构造一个 Client。discoveryFile 是服务发现文件路径(核心启动时写入,
// 记录当前核心监听地址),生产环境固定传 /var/run/nimoos/nimoos.url,
// 测试可传任意临时文件路径以便注入假核心地址。
func New(baseURLFile string) *Client {
	return &Client{
		discoveryFile: baseURLFile,
		httpClient:    &http.Client{Timeout: requestTimeout},
	}
}

// Upsert 增量推送一条 root 授权(新建或更新),对应核心端点
// PUT {base}/v1/nimoos/_internal/root-grants/{root_id}。
func (c *Client) Upsert(ctx context.Context, g Grant) error {
	body := struct {
		Path    string `json:"path"`
		Enabled bool   `json:"enabled"`
	}{Path: g.Path, Enabled: g.Enabled}
	url := fmt.Sprintf("%s/v1/nimoos/_internal/root-grants/%s", resolveBaseURL(c.discoveryFile), g.RootID)
	return c.do(ctx, http.MethodPut, url, body)
}

// Delete 删除一条 root 授权,对应核心端点
// DELETE {base}/v1/nimoos/_internal/root-grants/{root_id}。
func (c *Client) Delete(ctx context.Context, rootID string) error {
	url := fmt.Sprintf("%s/v1/nimoos/_internal/root-grants/%s", resolveBaseURL(c.discoveryFile), rootID)
	return c.do(ctx, http.MethodDelete, url, nil)
}

// Reconcile 全量对账推送(通常在 Wiki 服务启动时调用一次),对应核心端点
// POST {base}/v1/nimoos/_internal/root-grants/reconcile,body 为
// {"grants":[...]}。核心以此列表为准同步 source="wiki" 的所有行。
func (c *Client) Reconcile(ctx context.Context, grants []Grant) error {
	body := struct {
		Grants []Grant `json:"grants"`
	}{Grants: grants}
	url := fmt.Sprintf("%s/v1/nimoos/_internal/root-grants/reconcile", resolveBaseURL(c.discoveryFile))
	return c.do(ctx, http.MethodPost, url, body)
}

// do 是三个方法共用的请求发送逻辑:带短超时、JSON 编码请求体、非 2xx 状态码
// 一律转换为 error(不解析响应体细节,调用方只关心成功/失败)。
func (c *Client) do(ctx context.Context, method, url string, payload any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader *bytes.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("rootsync: 序列化请求体失败: %w", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("rootsync: 构造请求失败: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rootsync: 请求核心失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("rootsync: 核心返回非成功状态码 %d", resp.StatusCode)
	}
	return nil
}
