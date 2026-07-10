package domainutil

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// Normalize accepts a bare hostname or URL and returns a lowercase ASCII
// hostname suitable for a unique database key.
func Normalize(input string) (string, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", fmt.Errorf("域名不能为空")
	}

	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" {
			return "", fmt.Errorf("无效 URL")
		}
		value = parsed.Hostname()
	} else {
		value = strings.TrimSuffix(value, ".")
		if host, _, err := net.SplitHostPort(value); err == nil {
			value = host
		}
	}

	value = strings.Trim(strings.ToLower(value), "[]")
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil {
		return "", fmt.Errorf("域名 IDN 转换失败: %w", err)
	}
	if len(ascii) == 0 || len(ascii) > 253 || net.ParseIP(ascii) != nil {
		return "", fmt.Errorf("必须提供域名而不是 IP")
	}

	labels := strings.Split(ascii, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("域名必须包含顶级域")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("域名标签无效")
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return "", fmt.Errorf("域名包含无效字符")
			}
		}
	}
	return ascii, nil
}
