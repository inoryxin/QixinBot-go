package webui

import (
	"net"
	"net/url"
)

// 测试辅助：小写包装，避免测试文件里重复 import。
func parseIPForTest(s string) net.IP { return net.ParseIP(s) }

func urlParseForTest(raw string) (*url.URL, error) { return url.Parse(raw) }
