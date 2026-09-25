package sqlite

// Profile 是 PRAGMA 强度档位（第10.2章：预留 safe/balanced/performance，
// 默认 balanced）。
type Profile int

const (
	// ProfileSafe 最强持久性：每次提交 fsync，WAL 频繁自动 checkpoint，
	// 读连接少降低锁竞争。适合关键数据优先的场景。
	ProfileSafe Profile = iota
	// ProfileBalanced 默认档：synchronous=FULL + wal_autocheckpoint=1000
	// + busy_timeout=5000（第10.2章给出的基准值）。
	ProfileBalanced
	// ProfilePerformance 吞吐优先：synchronous=NORMAL（WAL 下掉电可能丢失
	// 最近已提交事务，但不会损坏数据库），更大的 checkpoint 间隔与读池。
	ProfilePerformance
)

func (p Profile) String() string {
	switch p {
	case ProfileSafe:
		return "safe"
	case ProfilePerformance:
		return "performance"
	default:
		return "balanced"
	}
}

// profileSpec 描述一个档位的具体参数。
type profileSpec struct {
	synchronous       string // OFF=0 / NORMAL=1 / FULL=2 / EXTRA=3
	busyTimeoutMS     int
	walAutocheckpoint int // 页数，约 4KB/页
	readConns         int
}

func (p Profile) spec() profileSpec {
	switch p {
	case ProfileSafe:
		return profileSpec{
			synchronous:       "FULL",
			busyTimeoutMS:     10000,
			walAutocheckpoint: 200,
			readConns:         2,
		}
	case ProfilePerformance:
		return profileSpec{
			synchronous:       "NORMAL",
			busyTimeoutMS:     15000,
			walAutocheckpoint: 4000,
			readConns:         8,
		}
	default:
		return profileSpec{
			synchronous:       "FULL",
			busyTimeoutMS:     5000,
			walAutocheckpoint: 1000,
			readConns:         4,
		}
	}
}

// journalSizeLimit 是所有档位共用的 WAL 大小上限（64MB）：阻止 WAL 在
// 读多写少场景下无限增长。
const journalSizeLimit = 64 * 1024 * 1024

// DefaultWriteQueueCap 是写队列容量上限（第45章：所有 queue 必须有容量上限）。
// 队列满时写入方阻塞等待（背压），而不是无限堆积。
const DefaultWriteQueueCap = 256
