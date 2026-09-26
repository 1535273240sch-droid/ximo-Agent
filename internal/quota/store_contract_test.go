package quota

import (
	"testing"

	gwstore "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
)

// accountStoreWiring 是编译期契约断言（§10.2）：各服务包的构造参数是本包自定义的
// 窄接口，而 *gwstore.Store 必须能**直接**传入 New，不需要适配器。
//
// 放在包级：store 侧一旦签名漂移（参数个数/类型/返回值变化，或方法被删），
// 这里会在编译期失败，而不是等到 HTTP 层接线时才发现。
var accountStoreWiring accountStore = (*gwstore.Store)(nil)

func TestStoreSatisfiesAccountStore(t *testing.T) {
	if accountStoreWiring == nil {
		t.Fatal("装配断言不应为 nil")
	}
	if svc := New(accountStoreWiring); svc == nil {
		t.Fatal("New 应返回可用服务")
	}
}
