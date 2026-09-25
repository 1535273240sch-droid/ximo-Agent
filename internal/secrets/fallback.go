package secrets

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// fallbackBackend 在首选后端写入失败时自动降级到备用后端。
//
// 为什么需要它：Windows 的 CredWriteW 只在**交互式登录会话**里能成功；
// 在服务账户、计划任务、容器或某些受限桌面会话下会返回失败。此时如果直接
// 报错，用户就会卡在"密钥存不进去"而无法使用整个应用——即使 DPAPI 文件后端
// 在同一台机器上完全可用（同样由系统主密钥加密，安全性等价）。
//
// 降级策略刻意只覆盖**写入**失败：读取时若首选后端没有该 ref，再问备用后端。
// 这样无论密钥当初存进了哪一个，后续都能取到，不会出现"存了但读不出来"。
type fallbackBackend struct {
	primary  Backend
	fallback Backend

	mu sync.Mutex
	// degraded 记录首选后端已经失败过，避免每次都先撞一次墙。
	degraded bool
}

// NewFallbackBackend 构造降级后端。两者都不可用时返回错误。
func NewFallbackBackend(primary, fallback Backend) (Backend, error) {
	if primary == nil && fallback == nil {
		return nil, ErrBackendUnavailable
	}
	if primary == nil {
		return fallback, nil
	}
	if fallback == nil {
		return primary, nil
	}
	return &fallbackBackend{primary: primary, fallback: fallback}, nil
}

func (b *fallbackBackend) Name() string {
	b.mu.Lock()
	degraded := b.degraded
	b.mu.Unlock()
	if degraded {
		return b.fallback.Name()
	}
	return b.primary.Name()
}

func (b *fallbackBackend) Available() bool {
	return b.primary.Available() || b.fallback.Available()
}

func (b *fallbackBackend) Get(ref string) (string, error) {
	// 先问首选（除非已知它坏了），再问备用。
	b.mu.Lock()
	degraded := b.degraded
	b.mu.Unlock()

	if !degraded {
		if v, err := b.primary.Get(ref); err == nil {
			return v, nil
		}
	}
	return b.fallback.Get(ref)
}

func (b *fallbackBackend) Put(ref, value string) error {
	b.mu.Lock()
	degraded := b.degraded
	b.mu.Unlock()

	if !degraded {
		if err := b.primary.Put(ref, value); err == nil {
			return nil
		} else if !b.fallback.Available() {
			// 备用也不可用：把首选的原始错误报出去，比含糊的"降级失败"更有用。
			return err
		} else {
			// 记住降级，后续写入直接走备用，省掉每次的失败往返。
			b.mu.Lock()
			b.degraded = true
			b.mu.Unlock()
		}
	}
	return b.fallback.Put(ref, value)
}

func (b *fallbackBackend) Delete(ref string) error {
	errPrimary := b.primary.Delete(ref)
	errFallback := b.fallback.Delete(ref)
	if errPrimary != nil && errFallback != nil {
		return fmt.Errorf("%v; %v", errPrimary, errFallback)
	}
	return nil
}

// windowsDefaultBackend 返回 Windows 上的默认后端：
// 优先 Credential Manager，配 DPAPI 文件后端作为自动降级目标。
//
// XIMO_SECRETS_BACKEND 可显式指定：
//
//	dpapi  —— 只用 DPAPI 文件后端
//	cred   —— 只用 Credential Manager
//	(空)   —— 首选 Credential Manager，失败时自动降级到 DPAPI
func windowsDefaultBackend() Backend {
	switch strings.ToLower(os.Getenv("XIMO_SECRETS_BACKEND")) {
	case "dpapi":
		if dpapi, err := NewDPAPIBackend(""); err == nil {
			return dpapi
		}
		return &CredentialBackend{}
	case "cred", "credential":
		return &CredentialBackend{}
	}

	cred := &CredentialBackend{}
	dpapi, err := NewDPAPIBackend("")
	if err != nil {
		// DPAPI 不可用（极罕见）：仍然只用 Credential Manager。
		return cred
	}
	combined, err := NewFallbackBackend(cred, dpapi)
	if err != nil {
		return cred
	}
	return combined
}
