// Package store_test 存放跨包**编译期契约断言**。
//
// 放在 store 的外部测试包里（而不是各消费方目录）：契约 §10.2 要求「实现方
// 结构性满足消费方的窄接口」，一旦签名漂移，这里会先于 HTTP 层调用方编译失败。
// 这三个被断言包都不反向依赖本测试包，因此不引入 import cycle。
package store_test

import (
	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	gwstore "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/upstream"
)

var (
	// account.Service 的必需存储能力必须由真实 store 直接满足。
	// （internal/account 侧的 integration_test.go 已有同样断言，这里是 store 侧的
	// 补充：改动 store 方法签名时本包同样会编译失败。）
	_ account.Store = (*gwstore.Store)(nil)
	// 设备码一次性消费（契约 §11.1.3）必须被 store 实现，否则 account 会静默降级
	// 为「进程内一次性保护」，跨进程/跨重启就会重复兑换。
	_ account.DeviceCodeConsumer = (*gwstore.Store)(nil)
	// catalog.Probe 由 upstream 池实现（熔断状态注入路由）。
	_ catalog.Probe = (*upstream.Pool)(nil)
)
