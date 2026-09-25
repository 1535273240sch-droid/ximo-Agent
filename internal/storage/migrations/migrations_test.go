package migrations

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

func newTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(t.TempDir(), "m.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func repoMigrations(t *testing.T) fstest.MapFS {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	m := fstest.MapFS{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		m[e.Name()] = &fstest.MapFile{Data: content}
	}
	if len(m) == 0 {
		t.Fatal("no migration files found in repo migrations/")
	}
	return m
}

func TestApplyFromRepoMigrations(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	r := NewRunner(db)
	if err := r.Load(repoMigrations(t)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	applied, err := r.Apply(ctx)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 2 || applied[0] != 1 || applied[1] != 2 {
		t.Fatalf("applied = %v, want [1 2]", applied)
	}

	cur, err := r.CurrentVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if cur != 2 {
		t.Errorf("current version = %d, want 2", cur)
	}

	// Repeated Apply is idempotent.
	applied2, err := r.Apply(ctx)
	if err != nil {
		t.Fatalf("Apply 2: %v", err)
	}
	if len(applied2) != 0 {
		t.Errorf("second apply = %v, want empty", applied2)
	}

	pending, err := r.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v, want empty", pending)
	}

	// user_version agrees with the registry table.
	var userVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if userVersion != 2 {
		t.Errorf("user_version = %d, want 2", userVersion)
	}

	// Registry row content.
	var name, sum string
	if err := db.QueryRowContext(ctx, "SELECT name, checksum FROM migrations WHERE version=1").Scan(&name, &sum); err != nil {
		t.Fatalf("read migrations row: %v", err)
	}
	if name != "init" || len(sum) != 64 {
		t.Errorf("row = (%q, %q), want (init, 64-hex)", name, sum)
	}

	// The schema really exists.
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables < 18 {
		t.Errorf("tables = %d, want >= 18", tables)
	}
}

func TestApplyDetectsChecksumTampering(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	r := NewRunner(db)
	if err := r.Load(repoMigrations(t)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := r.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Tamper with the registered checksum (simulating an unnoticed file edit).
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE migrations SET checksum='deadbeef' WHERE version=1")
		return err
	}); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := r.Apply(ctx); err == nil {
		t.Fatal("Apply should fail on checksum mismatch")
	}
}

func TestLoadRejectsVersionGap(t *testing.T) {
	db := newTestDB(t)
	r := NewRunner(db)
	fsys := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte("CREATE TABLE a (id INTEGER);")},
		"0003_c.sql": &fstest.MapFile{Data: []byte("CREATE TABLE c (id INTEGER);")},
	}
	if err := r.Load(fsys); err == nil {
		t.Fatal("Load should reject version gap 0001 -> 0003")
	}
}

func TestLoadRejectsEmptyDir(t *testing.T) {
	db := newTestDB(t)
	r := NewRunner(db)
	if err := r.Load(fstest.MapFS{}); err == nil {
		t.Fatal("Load should fail when no migration files exist")
	}
}

func TestApplyRollsBackFailedMigration(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	r := NewRunner(db)
	fsys := fstest.MapFS{
		"0001_ok.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE ok (id INTEGER PRIMARY KEY);")},
		"0002_bad.sql": &fstest.MapFile{Data: []byte("CREATE TABLE bad (id INTEGER); THIS IS NOT SQL;")},
	}
	if err := r.Load(fsys); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := r.Apply(ctx); err == nil {
		t.Fatal("Apply should fail on bad SQL")
	}
	// 0001 committed; 0002 must roll back completely: no registry row.
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM migrations WHERE version=2").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("failed migration 0002 was recorded: %d rows", count)
	}
	cur, _ := r.CurrentVersion(ctx)
	if cur != 1 {
		t.Errorf("current version = %d, want 1 (0002 rolled back)", cur)
	}
}

// extractCreateTable returns the normalized "CREATE TABLE <name> (...);"
// statement from a migration file, so the DDL can be compared against an
// external contract constant.
func extractCreateTable(sql, name string) string {
	needle := "CREATE TABLE " + name
	i := strings.Index(sql, needle)
	if i < 0 {
		return ""
	}
	rest := sql[i:]
	end := strings.Index(rest, ");")
	if end < 0 {
		return ""
	}
	return normalizeSQL(rest[:end+2])
}

// normalizeSQL strips -- comments and collapses whitespace so two DDL strings
// can be compared structurally.
func normalizeSQL(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

func TestApplyFromDir(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	dir := filepath.Join("..", "..", "..", "migrations")
	applied, err := ApplyFromDir(ctx, db, dir)
	if err != nil {
		t.Fatalf("ApplyFromDir: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want [1 2]", applied)
	}
}

// TestToolIdempotencyTableIsOwnedBy03 guards 08 ruling D-5: the
// tool_idempotency schema lives ONLY in this task's migrations/ (the
// architecture document's chapter 9 table list omits it while chapter 19
// defines it — a documentation gap that left nobody owning the table).
//
// The DDL must stay byte-identical to task 04's tool.IdempotencySchema so 04's
// SQL implementation keeps working unchanged.
func TestToolIdempotencyTableIsOwnedBy03(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	r := NewRunner(db)
	if err := r.Load(repoMigrations(t)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := r.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The table exists with exactly the four columns 04's DDL declares.
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('tool_idempotency')")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols = append(cols, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(cols) != 4 {
		t.Fatalf("tool_idempotency columns = %v, want exactly 4", cols)
	}
	want := map[string]bool{"key": false, "status": false, "result": false, "created_at": false}
	for _, c := range cols {
		if _, ok := want[c]; !ok {
			t.Errorf("unexpected column %q (must match 04's IdempotencySchema)", c)
			continue
		}
		want[c] = true
	}
	for c, seen := range want {
		if !seen {
			t.Errorf("missing column %q", c)
		}
	}

	// The DDL in the migration file is byte-identical to 04's IdempotencySchema
	// (modulo whitespace), so the two can never drift apart silently.
	var found bool
	for _, m := range r.Loaded() {
		if m.Name != "tool_idempotency" {
			continue
		}
		found = true
		// Compare only the CREATE TABLE statement (the file also carries the
		// stale-scan index, which 04's constant does not).
		table := extractCreateTable(m.SQL, "tool_idempotency")
		wantDDL := normalizeSQL(`CREATE TABLE tool_idempotency (
    key TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    result BLOB,
    created_at INTEGER NOT NULL
);`)
		if table != wantDDL {
			t.Errorf("0002 DDL drifted from 04's IdempotencySchema:\n got: %s\nwant: %s", table, wantDDL)
		}
	}
	if !found {
		t.Fatal("migrations/0002_tool_idempotency.sql is missing")
	}

	// The stale-scan index 04's Claim relies on exists.
	var idx string
	if err := db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='tool_idempotency' AND name LIKE 'idx_%'").
		Scan(&idx); err != nil {
		t.Fatalf("stale-scan index missing: %v", err)
	}
}
