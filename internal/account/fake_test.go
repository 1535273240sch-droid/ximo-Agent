package account

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// fakeStore 是 Store 的内存实现，语义与 internal/gateway/store 的约定对齐：
// 未命中返回 model.ErrNotFound，唯一约束冲突返回 model.ErrConflict，
// 轮换时旧行已吊销返回 model.ErrConflict。所有方法加同一把锁，模拟单写者。
type fakeStore struct {
	mu sync.Mutex

	users       map[string]model.User
	userNames   map[string]string
	keys        map[string]model.APIKey // by key hash
	keyHashes   map[string]string       // key id -> key hash
	sessions    map[string]model.AuthSession
	devices     map[string]model.DeviceCode // by device code hash
	deviceNames map[string]string           // user code -> device code hash

	// 故障注入：非 nil 时对应调用先记录再返回该错误。
	touchKeyErr     error
	touchDeviceErr  error
	rotateErr       error
	deviceConflicts int // >0 时 CreateDeviceCode 返回 ErrConflict 并递减

	touchKeyCalls    int
	touchDeviceCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:       make(map[string]model.User),
		userNames:   make(map[string]string),
		keys:        make(map[string]model.APIKey),
		keyHashes:   make(map[string]string),
		sessions:    make(map[string]model.AuthSession),
		devices:     make(map[string]model.DeviceCode),
		deviceNames: make(map[string]string),
	}
}

func (f *fakeStore) CreateUser(_ context.Context, u model.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.userNames[u.Username]; ok {
		return model.ErrConflict
	}
	if _, ok := f.users[u.ID]; ok {
		return model.ErrConflict
	}
	f.users[u.ID] = u
	f.userNames[u.Username] = u.ID
	return nil
}

func (f *fakeStore) GetUser(_ context.Context, id string) (model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return model.User{}, model.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) GetUserByName(_ context.Context, username string) (model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.userNames[username]
	if !ok {
		return model.User{}, model.ErrNotFound
	}
	return f.users[id], nil
}

func (f *fakeStore) CreateAPIKey(_ context.Context, k model.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keys[k.KeyHash]; ok {
		return model.ErrConflict
	}
	f.keys[k.KeyHash] = k
	f.keyHashes[k.ID] = k.KeyHash
	return nil
}

func (f *fakeStore) GetAPIKeyByHash(_ context.Context, hash string) (model.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[hash]
	if !ok {
		return model.APIKey{}, model.ErrNotFound
	}
	return k, nil
}

func (f *fakeStore) RevokeAPIKey(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.keyHashes[id]
	if !ok {
		return model.ErrNotFound
	}
	k := f.keys[hash]
	k.Status = model.KeyStatusRevoked
	f.keys[hash] = k
	return nil
}

func (f *fakeStore) TouchAPIKey(_ context.Context, id string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchKeyCalls++
	if f.touchKeyErr != nil {
		return f.touchKeyErr
	}
	hash, ok := f.keyHashes[id]
	if !ok {
		return model.ErrNotFound
	}
	k := f.keys[hash]
	k.LastUsedAt = at
	f.keys[hash] = k
	return nil
}

func (f *fakeStore) CreateAuthSession(_ context.Context, a model.AuthSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[a.ID]; ok {
		return model.ErrConflict
	}
	for _, s := range f.sessions {
		if s.RefreshHash == a.RefreshHash || s.AccessHash == a.AccessHash {
			return model.ErrConflict
		}
	}
	f.sessions[a.ID] = a
	return nil
}

func (f *fakeStore) GetAuthSessionByRefreshHash(_ context.Context, hash string) (model.AuthSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.RefreshHash == hash {
			return s, nil
		}
	}
	return model.AuthSession{}, model.ErrNotFound
}

func (f *fakeStore) GetAuthSessionByAccessHash(_ context.Context, hash string) (model.AuthSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.AccessHash == hash {
			return s, nil
		}
	}
	return model.AuthSession{}, model.ErrNotFound
}

// RotateAuthSession 与真实 store 同语义：同一「事务」里吊销旧行并插入 next。
// 旧行此前是否已吊销不参与判定（真实 store 也不判定），因此并发换发由服务层
// 的分片锁保证；next.ID 已存在则返回 model.ErrConflict（真实 store 会撞主键）。
func (f *fakeStore) RotateAuthSession(_ context.Context, oldID string, next model.AuthSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.sessions[oldID]
	if !ok {
		return model.ErrNotFound
	}
	if f.rotateErr != nil {
		return f.rotateErr
	}
	if _, exists := f.sessions[next.ID]; exists {
		return model.ErrConflict
	}
	if old.RevokedAt == 0 {
		old.RevokedAt = next.CreatedAt
	}
	f.sessions[oldID] = old
	f.sessions[next.ID] = next
	return nil
}

func (f *fakeStore) RevokeAuthSession(_ context.Context, id string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return model.ErrNotFound
	}
	s.RevokedAt = at
	f.sessions[id] = s
	return nil
}

func (f *fakeStore) CreateDeviceCode(_ context.Context, d model.DeviceCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deviceConflicts > 0 {
		f.deviceConflicts--
		return model.ErrConflict
	}
	if _, ok := f.devices[d.DeviceCodeHash]; ok {
		return model.ErrConflict
	}
	if _, ok := f.deviceNames[d.UserCode]; ok {
		return model.ErrConflict
	}
	f.devices[d.DeviceCodeHash] = d
	f.deviceNames[d.UserCode] = d.DeviceCodeHash
	return nil
}

func (f *fakeStore) GetDeviceCodeByDeviceHash(_ context.Context, hash string) (model.DeviceCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[hash]
	if !ok {
		return model.DeviceCode{}, model.ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) GetDeviceCodeByUserCode(_ context.Context, userCode string) (model.DeviceCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.deviceNames[userCode]
	if !ok {
		return model.DeviceCode{}, model.ErrNotFound
	}
	return f.devices[hash], nil
}

func (f *fakeStore) ApproveDeviceCode(_ context.Context, userCode, userID string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.deviceNames[userCode]
	if !ok {
		return model.ErrNotFound
	}
	d := f.devices[hash]
	if d.Status != model.DeviceStatusPending {
		return model.ErrConflict
	}
	d.UserID = userID
	d.Status = model.DeviceStatusApproved
	d.LastPolledAt = at
	f.devices[hash] = d
	return nil
}

func (f *fakeStore) TouchDeviceCodePoll(_ context.Context, hash string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchDeviceCalls++
	if f.touchDeviceErr != nil {
		return f.touchDeviceErr
	}
	d, ok := f.devices[hash]
	if !ok {
		return model.ErrNotFound
	}
	d.LastPolledAt = at
	f.devices[hash] = d
	return nil
}

// fakeConsumerStore 额外实现 DeviceCodeConsumer，用于验证数据库侧一次性消费。
type fakeConsumerStore struct {
	*fakeStore
	consumeErr error
}

func (f *fakeConsumerStore) ConsumeDeviceCode(_ context.Context, hash string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consumeErr != nil {
		return f.consumeErr
	}
	d, ok := f.devices[hash]
	if !ok {
		return model.ErrNotFound
	}
	if d.Status != model.DeviceStatusApproved {
		return model.ErrConflict
	}
	d.Status = model.DeviceStatusConsumed
	d.LastPolledAt = at
	f.devices[hash] = d
	return nil
}

// ---------------------------------------------------------------- 断言辅助

// liveSessions 返回未被吊销的会话。
func (f *fakeStore) liveSessions() []model.AuthSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.AuthSession
	for _, s := range f.sessions {
		if s.RevokedAt == 0 {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeStore) sessionByID(id string) (model.AuthSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	return s, ok
}

func (f *fakeStore) deviceByUserCode(code string) (model.DeviceCode, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.deviceNames[code]
	if !ok {
		return model.DeviceCode{}, false
	}
	return f.devices[hash], true
}

func (f *fakeStore) setUserStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return
	}
	u.Status = status
	f.users[id] = u
}

func (f *fakeStore) deviceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.devices)
}

// liveSessionCount 统计未被吊销的会话（两种假存储都支持）。
func liveSessionCount(st Store) int {
	switch v := st.(type) {
	case *fakeStore:
		return len(v.liveSessions())
	case *fakeConsumerStore:
		return len(v.liveSessions())
	default:
		return -1
	}
}

// storedStrings 汇总库中所有字符串字段，用于「明文不入库」断言。
func (f *fakeStore) storedStrings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, u := range f.users {
		out = append(out, u.ID, u.Username, u.PasswordHash, u.Status, u.GroupID)
	}
	for _, k := range f.keys {
		out = append(out, k.ID, k.UserID, k.KeyPrefix, k.KeyHash, k.Status)
	}
	for _, s := range f.sessions {
		out = append(out, s.ID, s.UserID, s.AccessHash, s.RefreshHash, s.RotatedFrom)
	}
	for _, d := range f.devices {
		out = append(out, d.DeviceCodeHash, d.UserCode, d.UserID, d.Status)
	}
	return out
}

// ---------------------------------------------------------------- 测试时钟

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.UnixMilli(1_700_000_000_000)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) ms() int64 { return c.now().UnixMilli() }

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestService 构造注入测试时钟的服务；pepper 为空串表示不使用 pepper。
func newTestService(st Store, pepper string) (*Service, *testClock) {
	c := newTestClock()
	s := New(st, []byte(pepper))
	s.now = c.now
	return s, c
}

// mustUser 创建一个可用用户并在失败时终止测试。
func mustUser(tb testing.TB, s *Service, username, password string) model.User {
	tb.Helper()
	u, err := s.CreateUser(context.Background(), username, password, "")
	if err != nil {
		tb.Fatalf("CreateUser(%q): %v", username, err)
	}
	return u
}
