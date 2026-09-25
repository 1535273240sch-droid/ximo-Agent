package unit

import (
	"errors"
	"testing"
)

type MigrationItem struct {
	ID   string
	Data string
}

type MigrationPlan struct {
	SourceItems []MigrationItem
	TargetDB    map[string]string
	Version     int
}

func ExecuteMigration(plan *MigrationPlan, failDuringImport bool) error {
	// 1. Backup old data (模拟内存快照)
	backup := make([]MigrationItem, len(plan.SourceItems))
	copy(backup, plan.SourceItems)

	// 2. Validate
	if len(plan.SourceItems) == 0 {
		return errors.New("empty source data")
	}

	// 3. Import in Transaction
	txTarget := make(map[string]string)
	for _, it := range plan.SourceItems {
		txTarget[it.ID] = it.Data
	}

	if failDuringImport {
		// 事务异常中止，不更新 TargetDB
		return errors.New("database error during migration transaction")
	}

	// 4. Verify counts
	if len(txTarget) != len(plan.SourceItems) {
		// 数量不符，拒绝提交
		return errors.New("count mismatch: data corruption detected")
	}

	// 5. Commit & mark version
	for k, v := range txTarget {
		plan.TargetDB[k] = v
	}
	plan.Version = 2
	return nil
}

func TestMigrationAtomicityAndCountVerification(t *testing.T) {
	source := []MigrationItem{
		{ID: "conv-1", Data: "chat history 1"},
		{ID: "conv-2", Data: "chat history 2"},
		{ID: "conv-3", Data: "chat history 3"},
	}

	// 1. 成功迁移
	plan := &MigrationPlan{
		SourceItems: source,
		TargetDB:    make(map[string]string),
		Version:     1,
	}
	if err := ExecuteMigration(plan, false); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	if len(plan.TargetDB) != 3 || plan.Version != 2 {
		t.Fatalf("expected 3 items migrated and version=2")
	}

	// 2. 失败回滚，目标数据库保持不动
	planFail := &MigrationPlan{
		SourceItems: source,
		TargetDB:    make(map[string]string),
		Version:     1,
	}
	err := ExecuteMigration(planFail, true)
	if err == nil {
		t.Fatalf("expected error during failed migration")
	}
	if len(planFail.TargetDB) != 0 || planFail.Version != 1 {
		t.Fatalf("expected rollback, target DB should be empty and version=1")
	}
}
