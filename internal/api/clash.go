package api

// Clash API 客户端:用于选中节点、测速、读取实时连接。
// sing-box 的 experimental.clash_api 与 Clash 的 REST 接口兼容,
// 因此 zashboard 可以直接作为内核面板使用,BoxHub 面板也复用同一套接口。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ClashClient struct {
	base   string
	secret string
	http   *http.Client
}

func NewClashClient(base, secret string) *ClashClient {
	return &ClashClient{
		base:   strings.TrimRight(base, "/"),
		secret: secret,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *ClashClient) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Clash API %s 返回 %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

type ProxyInfo struct {
	Type    string   `json:"type"`
	Now     string   `json:"now"`
	All     []string `json:"all"`
	History []struct {
		Time  time.Time `json:"time"`
		Delay int       `json:"delay"`
	} `json:"history"`
}

// Proxies 返回所有出站与当前选择。
func (c *ClashClient) Proxies(ctx context.Context) (map[string]ProxyInfo, error) {
	var out struct {
		Proxies map[string]ProxyInfo `json:"proxies"`
	}
	if err := c.do(ctx, "GET", "/proxies", &out); err != nil {
		return nil, err
	}
	return out.Proxies, nil
}

// Select 切换某个选择器组当前使用的出站。
func (c *ClashClient) Select(ctx context.Context, group, member string) error {
	body := fmt.Sprintf(`{"name":%q}`, member)
	req, err := http.NewRequestWithContext(ctx, "PUT", c.base+"/proxies/"+url.PathEscape(group), strings.NewReader(body))
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("切换失败: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

type DelayResult struct {
	Delay int    `json:"delay"`
	Error string `json:"error,omitempty"`
}

// Delay 经指定出站测速(内核会真实建立连接并访问 url)。
func (c *ClashClient) Delay(ctx context.Context, proxy, testURL string, timeout time.Duration) DelayResult {
	q := url.Values{}
	q.Set("url", testURL)
	q.Set("timeout", fmt.Sprint(timeout.Milliseconds()))
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.base+"/proxies/"+url.PathEscape(proxy)+"/delay?"+q.Encode(), nil)
	if err != nil {
		return DelayResult{Error: err.Error()}
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return DelayResult{Error: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return DelayResult{Error: strings.TrimSpace(string(body))}
	}
	var r DelayResult
	if err := json.Unmarshal(body, &r); err != nil {
		return DelayResult{Error: err.Error()}
	}
	return r
}

// Version 检查内核是否在运行并可接入。
func (c *ClashClient) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, "GET", "/version", &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

type ConnInfo struct {
	ID       string `json:"id"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
	Start    string `json:"start"`
	Chains   []string `json:"chains"`
	Rule     string `json:"rule"`
	RulePayload string `json:"rulePayload"`
	Meta     struct {
		Network  string `json:"network"`
		Host     string `json:"host"`
		DestIP   string `json:"destinationIP"`
		DestPort string `json:"destinationPort"`
		Process  string `json:"process"`
		SourceIP string `json:"sourceIP"`
	} `json:"metadata"`
}

// Connections 返回当前活跃连接。
func (c *ClashClient) Connections(ctx context.Context) ([]ConnInfo, error) {
	var out struct {
		Connections []ConnInfo `json:"connections"`
	}
	if err := c.do(ctx, "GET", "/connections", &out); err != nil {
		return nil, err
	}
	return out.Connections, nil
}

// RawGet 通用 GET,返回原始 JSON。用于 /connections 等未封装的接口。
func (c *ClashClient) RawGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Clash API %s 返回 %d", path, resp.StatusCode)
	}
	return body, nil
}
