package expert

import (
	"sync"
	"testing"
)

// countingStore 统计 Load 次数，用来证明注册表真的缓存住了索引。
type countingStore struct {
	mu     sync.Mutex
	loads  int
	shown  []Expert
	saves  int
	delete int
}

func (c *countingStore) Load() ([]Expert, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loads++
	out := make([]Expert, len(c.shown))
	copy(out, c.shown)
	return out, nil
}

func (c *countingStore) Save(e Expert) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves++
	for i, existing := range c.shown {
		if existing.ID == e.ID {
			c.shown[i] = e
			return nil
		}
	}
	c.shown = append(c.shown, e)
	return nil
}

func (c *countingStore) Delete(id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delete++
	for i, e := range c.shown {
		if e.ID == id {
			c.shown = append(c.shown[:i], c.shown[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (c *countingStore) loadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

// TestRegistryLoadCachesIndexUntilInvalidated 锁定「embed → lazy load → immutable」
// 这个承诺：Get/Search/Count 每次都会问 Load，而 Load 不能每次都重建 254 条索引。
//
// 复现的缺陷：Load 无条件重建，于是 SaveCustom/DeleteCustom 里那句「使缓存失效」
// 是死代码 —— 缓存从未生效，失效自然无意义；同时每次取一位专家都要付一次全量
// 索引重建的代价。
func TestRegistryLoadCachesIndexUntilInvalidated(t *testing.T) {
	store := &countingStore{}
	r := NewRegistry(store)

	if _, ok := r.Get("engineering-frontend-developer"); !ok {
		t.Fatal("内置专家查找失败，测试前提不成立")
	}
	if _, err := r.Count(); err != nil {
		t.Fatalf("Count: %v", err)
	}
	if _, err := r.Search("开发"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := store.loadCount(); got != 1 {
		t.Errorf("自定义存储被读取 %d 次，期望 1 次（索引应当缓存）", got)
	}

	// 保存自定义专家后必须失效，且新的专家立刻可见。
	custom := Expert{ID: "my-custom-expert", Division: "engineering", Name: "我的专家"}
	if err := r.SaveCustom(custom); err != nil {
		t.Fatalf("SaveCustom: %v", err)
	}
	got, ok := r.Get(custom.ID)
	if !ok {
		t.Fatal("保存后的自定义专家查不到：缓存没有失效")
	}
	if !got.Custom {
		t.Error("自定义专家没有被标记为 Custom")
	}
	if n := store.loadCount(); n != 2 {
		t.Errorf("失效后自定义存储被读取 %d 次，期望 2 次", n)
	}

	// 再读一次不应产生第三次读取。
	if _, ok := r.Get(custom.ID); !ok {
		t.Fatal("自定义专家第二次查找失败")
	}
	if n := store.loadCount(); n != 2 {
		t.Errorf("重复查找让自定义存储被读取 %d 次，期望仍是 2 次", n)
	}

	// 删除也要失效。
	ok, err := r.DeleteCustom(custom.ID)
	if err != nil || !ok {
		t.Fatalf("DeleteCustom = (%v, %v)，期望 (true, nil)", ok, err)
	}
	if _, still := r.Get(custom.ID); still {
		t.Error("删除后的自定义专家仍能查到：缓存没有失效")
	}
	if n := store.loadCount(); n != 3 {
		t.Errorf("删除后自定义存储被读取 %d 次，期望 3 次", n)
	}
}

// TestRegistryCachedLoadIsReadOnlyForCallers 确认 Load 的返回值是副本：
// 调用方改它不能污染注册表内部的索引（否则一次误改会让后续所有查找都错）。
func TestRegistryCachedLoadIsReadOnlyForCallers(t *testing.T) {
	r := NewRegistry(nil)

	first, err := r.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("内置专家库为空")
	}
	original := first[0].Name
	first[0].Name = "被调用方改坏了"

	second, err := r.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if second[0].Name != original {
		t.Errorf("Load 返回的是内部切片而不是副本：第二次读到 %q", second[0].Name)
	}
}
