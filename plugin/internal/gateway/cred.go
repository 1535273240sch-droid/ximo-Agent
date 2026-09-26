package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// credDirName / credFileName 组成默认凭据路径 <用户主目录>/.ximo-plugin/cred.json。
	credDirName  = ".ximo-plugin"
	credFileName = "cred.json"

	// credFilePerm / credDirPerm 是凭据文件与目录权限。
	//
	// Windows 上 os.Chmod 只影响只读位（实测 Go 1.27 下 chmod 0600 后 Stat 仍报
	// 0666），POSIX 权限位表达不了访问控制，因此 Windows 上这两次 Chmod 只是"尽力
	// 设置"，真实保护来自用户主目录的 ACL —— doctor 会据此报 WARN（见 PermStatus）。
	credFilePerm os.FileMode = 0o600
	credDirPerm  os.FileMode = 0o700
)

// Cred 是本地凭据文件的内容（默认 ~/.ximo-plugin/cred.json）。
//
// 只放两类东西：网关地址，以及用户凭据（长期 API 密钥 / 访问令牌 / 续期用的
// refresh token）。字段名沿用本插件早期版本（与网关登录链接口的返回字段同名：
// gateway/api_key/access_token/refresh_token/access_expires_at/...），旧凭据文件可直接读。
type Cred struct {
	// Gateway 是凭据所属的网关地址（规整后，无结尾 /）。
	Gateway string `json:"gateway"`
	// Username 仅用于展示；口令登录时会写入，设备码流程下网关不返回故为空。
	Username string `json:"username,omitempty"`
	// APIKey 是长期用户密钥（形如 ximo_sk_...），存在时优先作为调用网关的凭据。
	APIKey string `json:"api_key,omitempty"`
	// AccessToken / RefreshToken 是登录链得到的短期/续期令牌（gwa_/gwr_ 前缀）。
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// AccessExpiresAt / RefreshExpiresAt 是 unix 秒；0 表示未知。
	// 服务端的 tokenResponse 只有 access 的 expires_in，故 RefreshExpiresAt 目前
	// 恒为 0（保留字段以免将来加字段时破坏已存在的凭据文件）。
	AccessExpiresAt  int64 `json:"access_expires_at,omitempty"`
	RefreshExpiresAt int64 `json:"refresh_expires_at,omitempty"`
	UpdatedAt        int64 `json:"updated_at,omitempty"`
}

// Bearer 返回调用网关该带的凭据：API Key 优先（长期有效），否则 access token。
func (cred Cred) Bearer() string {
	if cred.APIKey != "" {
		return cred.APIKey
	}
	return cred.AccessToken
}

// Empty 判定凭据是否为空（既无密钥也无令牌）。
func (cred Cred) Empty() bool { return cred.Bearer() == "" }

// NeedsRefresh 判定 access token 是否需要续期：已过期或距过期不足 skew。
//
// 没有 access token、或没有到期时间（0 = 未知）时返回 false —— 未知不算过期，让
// 服务端的 401 来决定，避免把"服务端没给 expires_in"的凭据误判成过期而反复刷令牌
// （refresh 是轮换的，无谓的刷新会白白烧掉一个 refresh token）。
func (cred Cred) NeedsRefresh(now time.Time, skew time.Duration) bool {
	if cred.AccessToken == "" || cred.AccessExpiresAt == 0 {
		return false
	}
	return now.Add(skew).Unix() >= cred.AccessExpiresAt
}

// Fingerprint 是凭据的人类可读摘要（绝不含明文），供 login/models/doctor 输出。
func (cred Cred) Fingerprint() string {
	switch {
	case cred.APIKey != "" && cred.AccessToken != "":
		return fmt.Sprintf("API Key %s + access token %s", Mask(cred.APIKey), Mask(cred.AccessToken))
	case cred.APIKey != "":
		return "API Key " + Mask(cred.APIKey)
	case cred.AccessToken != "":
		return "access token " + Mask(cred.AccessToken)
	default:
		return "(无凭据)"
	}
}

// DefaultCredPath 返回默认凭据路径 ~/.ximo-plugin/cred.json。
func DefaultCredPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户主目录（可用 --home 覆盖）: %w", err)
	}
	return CredPathIn(home), nil
}

// CredPathIn 返回 home 下的凭据文件路径，供 --home 覆盖时使用。
func CredPathIn(home string) string {
	return filepath.Join(home, credDirName, credFileName)
}

// CredPath 返回本客户端实际使用的凭据路径（CredPath 为空时用默认路径）。
func (c *Client) ResolvedCredPath() (string, error) {
	if p := c.CredPath; p != "" {
		return p, nil
	}
	return DefaultCredPath()
}

// LoadCred 读取凭据文件。文件不存在返回 ErrNoCredential；JSON 破损按错误返回，
// 绝不静默当作"未登录"（否则用户会以为需要重新登录，而真实原因是被截断的文件）。
func (c *Client) LoadCred() (Cred, error) {
	path, err := c.ResolvedCredPath()
	if err != nil {
		return Cred{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Cred{}, fmt.Errorf("%w：%s 不存在，请先登录", ErrNoCredential, path)
		}
		return Cred{}, fmt.Errorf("读取凭据文件 %s 失败: %w", path, err)
	}
	var cred Cred
	if err := json.Unmarshal(raw, &cred); err != nil {
		return Cred{}, fmt.Errorf("凭据文件 %s 不是合法 JSON（已按失败处理，不会覆盖它）: %w", path, err)
	}
	return cred, nil
}

// SaveCred 原子写入凭据文件：先建临时文件并 chmod 0600 再写内容，最后 rename 覆盖，
// 避免进程中断留下半截 JSON，也避免"敏感内容先以宽权限落盘"。
func (c *Client) SaveCred(cred Cred) error {
	path, err := c.ResolvedCredPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, credDirPerm); err != nil {
		return fmt.Errorf("创建凭据目录 %s 失败: %w", dir, err)
	}
	// 目录权限也尽力收紧（Windows 上同样只是尽力）。
	if err := os.Chmod(dir, credDirPerm); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("设置凭据目录权限失败: %w", err)
	}

	cred.UpdatedAt = c.now().Unix()
	raw, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化凭据失败: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, ".cred-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时凭据文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(credFilePerm); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("设置凭据文件权限失败: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入凭据文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭凭据文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换凭据文件 %s 失败: %w", path, err)
	}
	c.logf("gateway: 凭据已写入 %s（%s）", path, cred.Fingerprint())
	return nil
}

// EnsureCred 取出可用凭据，必要时自动续期并回写。这是 CLI/engine 的推荐入口：
//
//	cred, err := client.EnsureCred(ctx)
//
// 行为：
//   - 凭据文件不存在 → ErrNoCredential；
//   - cred.Gateway 与 BaseURL 不一致 → ErrGatewayMismatch（避免把 A 网关的令牌
//     发给 B 网关）；
//   - 有长期 API Key → 直接用（不续期）；
//   - access 未过期（留 RefreshSkew 余量）→ 原样返回，不发任何请求；
//   - access 已过期且有 refresh → 调 /v1/auth/refresh 换新令牌并回写（轮换：旧
//     refresh 立即失效）；
//   - 续期失败 → 原样返回 LoadCred 的结果 + 错误（凭据被吊销/过期时需重新登录）。
//
// 注意一处刻意的不对称：**续期成功但回写失败**时，返回的 Cred 是已经续期的新凭据
// （本次调用可用），同时返回非 nil 错误 —— 因为新的 refresh token 只存在于内存里，
// 下次运行会用到失效的旧值，调用方必须把这件事告诉用户（重新登录以持久化）。
func (c *Client) EnsureCred(ctx context.Context) (Cred, error) {
	cred, err := c.LoadCred()
	if err != nil {
		return Cred{}, err
	}
	base, err := c.target()
	if err != nil {
		return cred, err
	}
	if cred.Gateway != "" && cred.Gateway != base {
		path, _ := c.ResolvedCredPath()
		return cred, fmt.Errorf("%w：%s 属于 %s，本次要访问 %s（请用对应的 --gateway 或重新登录）",
			ErrGatewayMismatch, path, cred.Gateway, base)
	}
	if cred.APIKey != "" {
		return cred, nil
	}
	if !cred.NeedsRefresh(c.now(), c.refreshSkew()) {
		return cred, nil
	}
	if cred.RefreshToken == "" {
		return cred, fmt.Errorf("%w：access token 已过期且没有 refresh_token，请重新登录", ErrCredentialExpired)
	}

	next, err := c.Refresh(ctx, cred)
	if err != nil {
		return cred, err
	}
	if err := c.SaveCred(next); err != nil {
		return next, fmt.Errorf("凭据已续期但写入失败（新 refresh 令牌只在本进程内有效，请重新登录以持久化）: %w", err)
	}
	return next, nil
}

func (c *Client) refreshSkew() time.Duration {
	if c.RefreshSkew > 0 {
		return c.RefreshSkew
	}
	return defaultRefreshSkew
}

// PermStatus 是凭据文件权限的体检结果（供 doctor 输出 PASS/WARN）。
type PermStatus struct {
	Path   string
	Exists bool
	// Mode 是 os.Stat 报出的权限位。Windows 上它只反映只读位，不代表真实访问控制。
	Mode os.FileMode
	// Secure 表示"已确认限制为 0600"。Windows 上恒为 false（无法验证）。
	Secure bool
	// Warning 非空表示 doctor 应报 WARN；空串表示无问题（文件不存在不算权限问题）。
	Warning string
}

// CheckCredPerm 检查凭据文件权限。Unix 上要求 mode&0o077 == 0（含目录）；Windows
// 上无法用权限位表达访问控制（os.Chmod 只影响只读位），故一律给出 WARN 说明，
// 由 doctor 按"警告"而非"失败"处理。
func CheckCredPerm(path string) PermStatus {
	st := PermStatus{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.Warning = fmt.Sprintf("无法读取凭据文件信息: %v", err)
		}
		return st
	}
	st.Exists = true
	st.Mode = info.Mode().Perm()

	if runtime.GOOS == "windows" {
		st.Warning = "Windows 不支持 POSIX 权限位：无法验证凭据文件为 0600（已尽力按最小权限创建）；" +
			"真实访问控制由所在用户目录的 ACL 提供，请不要把凭据文件放到共享目录"
		return st
	}
	if st.Mode&0o077 != 0 {
		st.Warning = fmt.Sprintf("凭据文件权限过宽（%04o，应为 0600）：请执行 chmod 600 %q", st.Mode, path)
		return st
	}
	st.Secure = true
	if dinfo, err := os.Stat(filepath.Dir(path)); err == nil {
		if d := dinfo.Mode().Perm(); d&0o077 != 0 {
			st.Warning = fmt.Sprintf("凭据目录权限过宽（%04o，应为 0700）：请执行 chmod 700 %q", d, filepath.Dir(path))
		}
	}
	return st
}

// CredPerm 检查本客户端凭据文件的权限，供 doctor 使用。
func (c *Client) CredPerm() (PermStatus, error) {
	path, err := c.ResolvedCredPath()
	if err != nil {
		return PermStatus{}, err
	}
	return CheckCredPerm(path), nil
}
