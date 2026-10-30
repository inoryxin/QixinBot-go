package webui

import (
	"context"
	"testing"
)

func TestCheckURLTargetBlocksInternal(t *testing.T) {
	bad := []string{
		"http://127.0.0.1:7801/overview",
		"http://localhost/admin",
		"http://192.0.2.10/x.png",
		"http://192.168.1.1/x.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/x.png",
		"http://example.com:8080/x.png",
		"ftp://example.com/x.png",
	}
	for _, raw := range bad {
		u, err := urlParseForTest(raw)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", raw, err)
		}
		if err := checkURLTarget(u); err == nil {
			t.Errorf("%s 应该被拒绝", raw)
		}
	}
}

func TestFetchImageRejectsLocal(t *testing.T) {
	if _, _, err := fetchImage(context.Background(), "http://127.0.0.1:7801/overview"); err == nil {
		t.Fatal("本机地址应该被拒绝")
	}
	if _, _, err := fetchImage(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("file:// 应该被拒绝")
	}
}

func TestIPBlocked(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.0.1",
		"169.254.169.254", "100.64.0.1", "::1", "fe80::1", "fc00::1", "0.0.0.0"}
	for _, s := range blocked {
		ip := parseIPForTest(s)
		if ip == nil || !ipBlocked(ip) {
			t.Errorf("%s 应判定为禁止地址", s)
		}
	}
	if ip := parseIPForTest("1.1.1.1"); ipBlocked(ip) {
		t.Error("1.1.1.1 不应该被拦")
	}
	if ip := parseIPForTest("2606:4700:4700::1111"); ipBlocked(ip) {
		t.Error("公网 IPv6 不应该被拦")
	}
}
