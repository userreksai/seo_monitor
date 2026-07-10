package domainfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"seo-monitor/internal/domainutil"
)

const maxDomains = 10000

// Load reads domains from a JSON array, a {"domains": [...]} object, or the
// relaxed comma/newline format shown in the project documentation.
func Load(filename string) ([]string, error) {
	body, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("域名文件为空")
	}

	values, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", filename, err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s 中没有域名", filename)
	}
	if len(values) > maxDomains {
		return nil, fmt.Errorf("%s 最多允许 %d 个域名", filename, maxDomains)
	}

	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		domain, normalizeErr := domainutil.Normalize(value)
		if normalizeErr != nil {
			return nil, fmt.Errorf("第 %d 项 %q 无效: %w", index+1, value, normalizeErr)
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		result = append(result, domain)
	}
	return result, nil
}

func decode(body []byte) ([]string, error) {
	var array []string
	if err := json.Unmarshal(body, &array); err == nil && array != nil {
		return array, nil
	}

	var object struct {
		Domains []string `json:"domains"`
	}
	if err := json.Unmarshal(body, &object); err == nil && object.Domains != nil {
		return object.Domains, nil
	}

	if json.Valid(body) {
		return nil, errors.New("JSON 必须是字符串数组或包含 domains 数组的对象")
	}

	// Compatibility with the user's relaxed example: { 123.com, 222.com, }
	parts := strings.FieldsFunc(string(body), func(r rune) bool {
		switch r {
		case ',', '\n', '\r', '{', '}', '[', ']':
			return true
		default:
			return false
		}
	})
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.Trim(strings.TrimSpace(part), "\"'")
		if value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return nil, errors.New("无法识别域名列表")
	}
	return values, nil
}
