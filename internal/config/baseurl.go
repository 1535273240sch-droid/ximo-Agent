// baseurl.go —— 用户手填接口地址的规整与校验。
//
// 界面上的 Base URL 是用户手敲的，常见形态问题：前后空格、忘了 https://、
// 结尾多一个斜杠、把完整接口路径当 BaseURL。这些问题直接透传会以很隐晦的
// 方式失败（获取模型 404 / 构造请求失败 / chat 全部报错），必须在入库和
// 发请求前统一规整，规整不了就给出人话错误。
package config

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeBaseURL 规整用户填写的服务商接口地址：
//   - 去首尾空白与结尾斜杠；
//   - 没有 scheme 时自动补 https://；
//   - 校验 scheme 必须是 http/https 且主机名非空。
//
// 返回规整后的地址；无法规整（缺主机名、非 http/https scheme）时返回错误。
// raw 为空时返回空串（由调用方决定"BaseURL 必填"的报错口径）。
func NormalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("接口地址无法解析（%q）：%v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("接口地址的协议必须是 http(s)，当前为 %q", u.Scheme+"://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("接口地址缺少主机名（例如 https://api.example.com/v1）")
	}
	return strings.TrimRight(s, "/"), nil
}
