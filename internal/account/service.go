package account

import (
	"hash/fnv"
	"sync"
	"time"
)

const (
	// DefaultAccessTTL 是 access token 有效期（短期凭据）。
	DefaultAccessTTL = 15 * time.Minute
	// DefaultRefreshTTL 是会话总寿命：refresh 轮换只换发新 access，不延长这个
	// 绝对期限，避免一个被盗 refresh 无限续命（轮换只能减小暴露面）。
	DefaultRefreshTTL = 30 * 24 * time.Hour
	// DefaultDeviceCodeTTL 是设备码有效期（文档 §5.2）。
	DefaultDeviceCodeTTL = 10 * time.Minute
	// DefaultPollIntervalMS 是设备轮询间隔建议值（OAuth device flow 的 interval）。
	DefaultPollIntervalMS = 5000

	// maxUserCodeAttempts 是用户码与库中既有码撞车后的重试次数。
	maxUserCodeAttempts = 5
	// lockStripes 是会话/设备码状态机的串行化分片数。
	lockStripes = 64
	// usedDeviceCodePruneAt 是降级消费表的清理阈值。
	usedDeviceCodePruneAt = 256
)

// Service 是账号与凭据服务。零依赖全局状态，可安全并发使用。
//
// pepper 是服务端侧的秘密（例如由 internal/secrets 提供）：非空时所有口令先经
// HMAC-SHA256(pepper, password) 再进 PBKDF2，使「只泄漏数据库」不足以离线爆破。
// pepper 为空则退化为标准 PBKDF2（测试/单机场景）。
//
// 设备码一次性消费：store 实现 DeviceCodeConsumer 时以数据库状态为准；否则本
// 进程用 usedDeviceCodes 兜底（跨进程/重启前有效期为设备码本身的有效期）。降级
// 行为不满足「跨进程一次性」，属于已知缺口，需要 store 侧补 ConsumeDeviceCode。
type Service struct {
	st       Store
	pepper   []byte
	consumer DeviceCodeConsumer
	locks    keyedLocks
	used     usedDeviceCodes
	// now 可注入（包内测试用），生产路径固定为 time.Now。
	now func() time.Time
}

// New 构造服务。st 为 nil 是编程错误，直接 panic 而不是让后续每个调用各自崩。
func New(st Store, pepper []byte) *Service {
	if st == nil {
		panic("account: store 不能为 nil")
	}
	s := &Service{st: st, now: time.Now}
	if len(pepper) > 0 {
		s.pepper = append([]byte(nil), pepper...)
	}
	if c, ok := st.(DeviceCodeConsumer); ok {
		s.consumer = c
	}
	return s
}

func (s *Service) nowMS() int64 { return s.now().UnixMilli() }

// keyedLocks 按 key 分片加锁：同一 refresh token / 同一设备码的并发处理被串行化，
// 否则「同一 refresh 并发换发」可能产生两个有效会话（DB 层的条件更新是第二道
// 防线，见 RotateAuthSession 的接口约定）。
type keyedLocks struct{ m [lockStripes]sync.Mutex }

func (k *keyedLocks) lock(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	mu := &k.m[h.Sum32()%lockStripes]
	mu.Lock()
	return mu
}

// usedDeviceCodes 是 store 未实现 DeviceCodeConsumer 时的降级一次性保护：
// 记为已消费的设备码在设备码自身过期前不可再次兑换。
type usedDeviceCodes struct {
	mu sync.Mutex
	m  map[string]int64 // device code hash -> 过期时间（unix ms）
}

func (u *usedDeviceCodes) claim(codeHash string, now, expiresAt int64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = make(map[string]int64)
	}
	if len(u.m) >= usedDeviceCodePruneAt {
		for k, at := range u.m {
			if at <= now {
				delete(u.m, k)
			}
		}
	}
	if at, ok := u.m[codeHash]; ok && at > now {
		return false
	}
	u.m[codeHash] = expiresAt
	return true
}
