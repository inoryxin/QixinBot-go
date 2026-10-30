package webui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 图片代理的安全边界：
//   - 只允许 http/https，端口只允许 80/443
//   - 目标地址解析后必须是公网 IP（拒绝 loopback/私网/链路本地/元数据/组播）
//   - 重定向逐跳重新校验（最多 3 跳）
//   - 只返回图片（嗅探内容），固定 Content-Type + nosniff

const maxImageBytes = 10 << 20

func ipBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// 云元数据/特殊用途网段
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 169 && v4[1] == 254: // link-local（含 169.254.169.254）
			return true
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // CGNAT
			return true
		case v4[0] == 0:
			return true
		}
	}
	return false
}

func checkURLTarget(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只允许 http/https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("缺少主机名")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if port != "80" && port != "443" {
		return fmt.Errorf("只允许 80/443 端口")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("域名解析失败")
	}
	for _, ipa := range ips {
		if ipBlocked(ipa.IP) {
			return fmt.Errorf("目标地址不允许（内网/本机/元数据）")
		}
	}
	return nil
}

// safeDial 在真正连接时再校验一次解析结果，降低 DNS rebinding 风险。
func safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if port != "80" && port != "443" {
		return nil, errors.New("只允许 80/443 端口")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("域名解析失败: %w", err)
	}
	var lastErr error
	for _, ipa := range ips {
		if ipBlocked(ipa.IP) {
			lastErr = errors.New("目标地址不允许")
			continue
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用地址")
	}
	return nil, lastErr
}

func imageClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:           safeDial,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("重定向次数过多")
			}
			return checkURLTarget(req.URL)
		},
	}
}

// fetchImage 拉取并校验图片，返回内容和嗅探出的类型。
func fetchImage(ctx context.Context, raw string) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("地址不合法")
	}
	if err := checkURLTarget(u); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/*")
	resp, err := imageClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("上游返回 %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxImageBytes {
		return nil, "", fmt.Errorf("图片太大")
	}
	ct := http.DetectContentType(b)
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("不是图片内容（%s）", ct)
	}
	return b, ct, nil
}
