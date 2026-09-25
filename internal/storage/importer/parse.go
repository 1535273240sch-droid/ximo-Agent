package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
)

// legacySource 是一个待迁移的源文件。
type legacySource struct {
	// kind 决定解析方式。
	kind string
	// path 绝对路径。
	path string
}

// discover 扫描 legacy 目录，识别所有可迁移的 v1 数据文件。
func (im *Importer) discover() ([]legacySource, error) {
	var out []legacySource
	add := func(kind, path string) {
		out = append(out, legacySource{kind: kind, path: path})
	}

	// 顶层文件
	for _, f := range []struct {
		kind string
		name string
	}{
		{"settings", "settings.json"},
		{"conversations", "conversations.json"},
		{"skills", "skills.json"},
		{"imported_skills", "imported-skills.json"},
		{"mcp", "mcp-config.json"},
		{"experts", "experts.json"},
		{"ui_components", "ui-components-catalog.json"},
	} {
		p := filepath.Join(im.legacyDir, f.name)
		if fileExists(p) {
			add(f.kind, p)
		}
	}

	// memory/<mode>.md
	if files, err := filepath.Glob(filepath.Join(im.legacyDir, "memory", "*.md")); err == nil {
		sort.Strings(files)
		for _, p := range files {
			add("memory", p)
		}
	}

	// knowledge/<mode>/entries.json
	if files, err := filepath.Glob(filepath.Join(im.legacyDir, "knowledge", "*", "entries.json")); err == nil {
		sort.Strings(files)
		for _, p := range files {
			add("knowledge", p)
		}
	}

	// themes/*.json
	if files, err := filepath.Glob(filepath.Join(im.legacyDir, "themes", "*.json")); err == nil {
		sort.Strings(files)
		for _, p := range files {
			add("theme", p)
		}
	}

	// backgrounds/*（图片/视频资产，按文件计数）
	if files, err := filepath.Glob(filepath.Join(im.legacyDir, "backgrounds", "*")); err == nil {
		sort.Strings(files)
		for _, p := range files {
			if fileExists(p) {
				add("background", p)
			}
		}
	}

	// design-styles/<id>/（目录形式资产）
	if dirs, err := os.ReadDir(filepath.Join(im.legacyDir, "design-styles")); err == nil {
		var names []string
		for _, d := range dirs {
			if d.IsDir() {
				names = append(names, d.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			add("design_style", filepath.Join(im.legacyDir, "design-styles", n))
		}
	}

	// ui-components/<cat>/<id>/（目录形式资产）
	if cats, err := os.ReadDir(filepath.Join(im.legacyDir, "ui-components")); err == nil {
		var paths []string
		for _, c := range cats {
			if !c.IsDir() {
				continue
			}
			ids, err := os.ReadDir(filepath.Join(im.legacyDir, "ui-components", c.Name()))
			if err != nil {
				continue
			}
			for _, id := range ids {
				if id.IsDir() {
					paths = append(paths, filepath.Join(im.legacyDir, "ui-components", c.Name(), id.Name()))
				}
			}
		}
		sort.Strings(paths)
		for _, p := range paths {
			add("ui_component", p)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 解析结果

// legacyData 是全部解析后的待导入数据（内存中的“已验证”形态）。
type legacyData struct {
	settings       []byte
	conversations  []legacyConversation
	skills         []legacySkill
	importedSkills []legacySkill
	mcpServers     []legacyMcpServer
	experts        []legacyExpert
	memory         []legacyMemory
	knowledge      []legacyKnowledge
	themes         []legacyTheme
	assets         []legacyAsset
}

type legacyConversation struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Mode      string          `json:"mode"`
	Messages  []legacyMessage `json:"messages"`
	CreatedAt int64           `json:"createdAt"`
	UpdatedAt int64           `json:"updatedAt"`
}

type legacyMessage struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
	Tokens    int64  `json:"tokens"`
}

type legacySkill struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Steps       json.RawMessage `json:"steps"`
}

type legacyMcpServer struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Enabled   bool              `json:"enabled"`
}

type legacyExpert struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Prompt string   `json:"prompt"`
	Model  string   `json:"model"`
	Tools  []string `json:"tools"`
}

type legacyMemory struct {
	Mode    string
	Content string
}

type legacyKnowledge struct {
	Mode  string
	Entry struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Content   string   `json:"content"`
		Tags      []string `json:"tags"`
		Source    string   `json:"source"`
		CreatedAt int64    `json:"createdAt"`
		UpdatedAt int64    `json:"updatedAt"`
	}
}

type legacyTheme struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Raw  json.RawMessage `json:"-"`
}

type legacyAsset struct {
	Kind string // background / design_style / ui_component / ui_components
	ID   string
	Path string
	Raw  json.RawMessage
}

// parseAll 解析全部源文件，做结构校验并计算 checksum，统计写入 rep。
func (im *Importer) parseAll(ctx context.Context, sources []legacySource, rep *Report) (*legacyData, error) {
	data := &legacyData{}
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Stat(src.path)
		if err != nil {
			return nil, fmt.Errorf("importer: stat %s: %w", src.path, err)
		}
		stat := SourceStat{Path: src.path, Bytes: info.Size()}

		switch src.kind {
		case "settings":
			raw, err := os.ReadFile(src.path)
			if err != nil {
				return nil, fmt.Errorf("importer: read %s: %w", src.path, err)
			}
			if !json.Valid(raw) {
				return nil, fmt.Errorf("importer: %s is not valid JSON", src.path)
			}
			data.settings = raw
			stat.Records = 1

		case "conversations":
			var convs []legacyConversation
			if _, err := readJSONFile(src.path, &convs); err != nil {
				return nil, fmt.Errorf("importer: %w", err)
			}
			for _, c := range convs {
				if c.ID == "" {
					return nil, fmt.Errorf("importer: %s has conversation without id", src.path)
				}
			}
			data.conversations = convs
			stat.Records = len(convs)

		case "skills":
			var skills []legacySkill
			if _, err := readJSONFile(src.path, &skills); err != nil {
				return nil, fmt.Errorf("importer: %w", err)
			}
			for _, s := range skills {
				if s.ID == "" {
					return nil, fmt.Errorf("importer: %s has skill without id", src.path)
				}
			}
			data.skills = skills
			stat.Records = len(skills)

		case "imported_skills":
			var skills []legacySkill
			if _, err := readJSONFile(src.path, &skills); err != nil {
				return nil, fmt.Errorf("importer: %w", err)
			}
			data.importedSkills = skills
			stat.Records = len(skills)

		case "mcp":
			var servers []legacyMcpServer
			if _, err := readJSONFile(src.path, &servers); err != nil {
				return nil, fmt.Errorf("importer: %w", err)
			}
			for _, s := range servers {
				if s.ID == "" {
					return nil, fmt.Errorf("importer: %s has mcp server without id", src.path)
				}
				if s.Command == "" && s.URL == "" {
					return nil, fmt.Errorf("importer: %s: mcp server %s missing command/url", src.path, s.ID)
				}
			}
			data.mcpServers = servers
			stat.Records = len(servers)

		case "experts":
			var experts []legacyExpert
			if _, err := readJSONFile(src.path, &experts); err != nil {
				return nil, fmt.Errorf("importer: %w", err)
			}
			data.experts = experts
			stat.Records = len(experts)

		case "memory":
			content, err := os.ReadFile(src.path)
			if err != nil {
				return nil, fmt.Errorf("importer: read %s: %w", src.path, err)
			}
			mode := strings.TrimSuffix(filepath.Base(src.path), ".md")
			data.memory = append(data.memory, legacyMemory{Mode: mode, Content: string(content)})
			stat.Records = 1

		case "knowledge":
			var entries []struct {
				ID        string   `json:"id"`
				Title     string   `json:"title"`
				Content   string   `json:"content"`
				Tags      []string `json:"tags"`
				Source    string   `json:"source"`
				CreatedAt int64    `json:"createdAt"`
				UpdatedAt int64    `json:"updatedAt"`
			}
			if _, err := readJSONFile(src.path, &entries); err != nil {
				return nil, fmt.Errorf("importer: %w", err)
			}
			mode := filepath.Base(filepath.Dir(src.path))
			for _, e := range entries {
				var k legacyKnowledge
				k.Mode = mode
				k.Entry.ID = e.ID
				k.Entry.Title = e.Title
				k.Entry.Content = e.Content
				k.Entry.Tags = e.Tags
				k.Entry.Source = e.Source
				k.Entry.CreatedAt = e.CreatedAt
				k.Entry.UpdatedAt = e.UpdatedAt
				if k.Entry.ID == "" {
					return nil, fmt.Errorf("importer: %s has knowledge entry without id", src.path)
				}
				data.knowledge = append(data.knowledge, k)
			}
			stat.Records = len(entries)

		case "theme":
			raw, err := os.ReadFile(src.path)
			if err != nil {
				return nil, fmt.Errorf("importer: read %s: %w", src.path, err)
			}
			if !json.Valid(raw) {
				return nil, fmt.Errorf("importer: %s is not valid JSON", src.path)
			}
			id := strings.TrimSuffix(filepath.Base(src.path), ".json")
			data.themes = append(data.themes, legacyTheme{ID: id, Raw: raw})
			stat.Records = 1

		case "background":
			data.assets = append(data.assets, legacyAsset{
				Kind: "background",
				ID:   strings.TrimSuffix(filepath.Base(src.path), filepath.Ext(src.path)),
				Path: src.path,
			})
			stat.Records = 1

		case "design_style":
			data.assets = append(data.assets, legacyAsset{
				Kind: "design_style",
				ID:   filepath.Base(src.path),
				Path: src.path,
			})
			stat.Records = 1

		case "ui_component":
			rel, _ := filepath.Rel(filepath.Join(im.legacyDir, "ui-components"), src.path)
			data.assets = append(data.assets, legacyAsset{
				Kind: "ui_component",
				ID:   filepath.ToSlash(rel),
				Path: src.path,
			})
			stat.Records = 1

		case "ui_components":
			raw, err := os.ReadFile(src.path)
			if err != nil {
				return nil, fmt.Errorf("importer: read %s: %w", src.path, err)
			}
			var catalog []map[string]any
			if err := json.Unmarshal(raw, &catalog); err != nil {
				return nil, fmt.Errorf("importer: parse %s: %w", src.path, err)
			}
			data.assets = append(data.assets, legacyAsset{
				Kind: "ui_components",
				ID:   "catalog",
				Raw:  raw,
			})
			stat.Records = len(catalog)

		default:
			return nil, fmt.Errorf("importer: unknown source kind %q", src.kind)
		}

		stat.Checksum = checksumFile(src.path)
		rep.Sources[src.path] = stat
	}
	return data, nil
}

// ---------------------------------------------------------------- 导入

// importAndVerify 在单事务内导入并核对数量。数量不符返回错误 → 事务回滚，
// 旧数据与已导入数据都不留下中间态。
func (im *Importer) importAndVerify(ctx context.Context, tx storage.Tx, data *legacyData, rep *Report) error {
	now := im.now().UnixMilli()

	// ---- settings → kv
	if len(data.settings) > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kv (key, value, updated_at) VALUES ('settings', ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			data.settings, now); err != nil {
			return fmt.Errorf("importer: import settings: %w", err)
		}
		rep.Imported["kv.settings"] = 1
	}

	// ---- memory → kv（memory:<mode>）
	for _, m := range data.memory {
		key := "memory:" + m.Mode
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, []byte(m.Content), now); err != nil {
			return fmt.Errorf("importer: import memory %s: %w", m.Mode, err)
		}
	}
	rep.Imported["kv.memory"] = len(data.memory)

	// ---- conversations → sessions + (legacy run) + messages
	for _, c := range data.conversations {
		mode := c.Mode
		if mode == "" {
			mode = "default"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES (?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET title=excluded.title, mode=excluded.mode, updated_at=excluded.updated_at`,
			c.ID, c.Title, mode, c.CreatedAt, c.UpdatedAt); err != nil {
			return fmt.Errorf("importer: import session %s: %w", c.ID, err)
		}
		// v1 会话没有 run 概念；为历史消息挂一个 synthetic legacy run
		runID := "legacy-" + c.ID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runs (id, session_id, status, prompt, created_at, updated_at, finished_at, last_seq, idempotency_key)
			 VALUES (?,?, 'completed', ?, ?, ?, ?, 0, ?)
			 ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at`,
			runID, c.ID, c.Title, c.CreatedAt, c.UpdatedAt, c.UpdatedAt, "legacy:"+c.ID); err != nil {
			return fmt.Errorf("importer: import legacy run %s: %w", runID, err)
		}
		for i, m := range c.Messages {
			role := m.Role
			if role == "" {
				role = "user"
			}
			msgID := m.ID
			if msgID == "" {
				msgID = fmt.Sprintf("%s-msg-%d", c.ID, i)
			}
			ts := m.Timestamp
			if ts == 0 {
				ts = c.CreatedAt
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO messages (id, run_id, session_id, role, content, seq, created_at, token_count)
				 VALUES (?,?,?,?,?,?,?,?)
				 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
				msgID, runID, c.ID, role, m.Content, int64(i)+1, ts, nullInt(m.Tokens)); err != nil {
				return fmt.Errorf("importer: import message %s: %w", msgID, err)
			}
		}
	}
	rep.Imported["sessions"] = len(data.conversations)
	rep.Imported["messages"] = 0
	for _, c := range data.conversations {
		rep.Imported["messages"] += len(c.Messages)
	}

	// ---- skills + imported-skills → skills
	for _, s := range data.skills {
		if err := importSkill(ctx, tx, s, "builtin", now); err != nil {
			return err
		}
	}
	for _, s := range data.importedSkills {
		if err := importSkill(ctx, tx, s, "imported", now); err != nil {
			return err
		}
	}
	rep.Imported["skills"] = len(data.skills) + len(data.importedSkills)

	// ---- mcp-config.json → mcp_servers
	for _, s := range data.mcpServers {
		args, _ := json.Marshal(s.Args)
		env, _ := json.Marshal(s.Env)
		headers, _ := json.Marshal(s.Headers)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mcp_servers (id, name, transport, command, args_json, env_json, url, headers_json, enabled, imported_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, transport=excluded.transport, command=excluded.command,
			   args_json=excluded.args_json, env_json=excluded.env_json, url=excluded.url,
			   headers_json=excluded.headers_json, enabled=excluded.enabled`,
			s.ID, s.Name, orDefault(s.Transport, "stdio"), orDefault(s.Command, ""), args, env, orDefault(s.URL, ""),
			headers, boolToInt64(s.Enabled), now); err != nil {
			return fmt.Errorf("importer: import mcp server %s: %w", s.ID, err)
		}
	}
	rep.Imported["mcp_servers"] = len(data.mcpServers)

	// ---- experts.json → experts
	for _, e := range data.experts {
		tools, _ := json.Marshal(e.Tools)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO experts (id, name, description, prompt, model, tools_json, enabled, source, created_at, updated_at)
			 VALUES (?,?, '', ?, ?, ?, 1, 'legacy', ?, ?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, prompt=excluded.prompt, model=excluded.model,
			   tools_json=excluded.tools_json, updated_at=excluded.updated_at`,
			e.ID, e.Name, orDefault(e.Prompt, ""), orDefault(e.Model, ""), tools, now, now); err != nil {
			return fmt.Errorf("importer: import expert %s: %w", e.ID, err)
		}
	}
	rep.Imported["experts"] = len(data.experts)

	// ---- knowledge/<mode>/entries.json → knowledge_entries
	for _, k := range data.knowledge {
		tags, _ := json.Marshal(k.Entry.Tags)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO knowledge_entries (id, mode, title, content, tags_json, source, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET mode=excluded.mode, title=excluded.title, content=excluded.content,
			   tags_json=excluded.tags_json, source=excluded.source, updated_at=excluded.updated_at`,
			k.Entry.ID, k.Mode, k.Entry.Title, k.Entry.Content, tags, orDefault(k.Entry.Source, "legacy"),
			orDefault64(k.Entry.CreatedAt, now), orDefault64(k.Entry.UpdatedAt, now)); err != nil {
			return fmt.Errorf("importer: import knowledge %s: %w", k.Entry.ID, err)
		}
	}
	rep.Imported["knowledge_entries"] = len(data.knowledge)

	// ---- themes / 资产 → kv（元数据）+ 原路径引用
	for _, t := range data.themes {
		if err := setKVInTx(ctx, tx, "theme:"+t.ID, t.Raw, now); err != nil {
			return err
		}
	}
	for _, a := range data.assets {
		key := a.Kind + ":" + a.ID
		val := []byte(a.Path)
		if len(a.Raw) > 0 {
			val = a.Raw
		}
		if err := setKVInTx(ctx, tx, key, val, now); err != nil {
			return err
		}
	}
	rep.Imported["kv.themes"] = len(data.themes)
	rep.Imported["kv.assets"] = len(data.assets)

	// ---- 数量核对：DB 实际行数必须与解析数量一致
	return im.verifyCounts(ctx, tx, data, rep)
}

func importSkill(ctx context.Context, tx storage.Tx, s legacySkill, source string, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO skills (id, name, description, source, steps_json, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,?, 1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, description=excluded.description,
		   source=excluded.source, steps_json=excluded.steps_json, updated_at=excluded.updated_at`,
		s.ID, orDefault(s.Name, s.ID), orDefault(s.Description, ""), source, orDefaultJSON(s.Steps), now, now); err != nil {
		return fmt.Errorf("importer: import skill %s: %w", s.ID, err)
	}
	return nil
}

// verifyCounts 在同事务内核对每类数据的实际行数。
func (im *Importer) verifyCounts(ctx context.Context, tx storage.Tx, data *legacyData, rep *Report) error {
	type check struct {
		table string
		query string
		args  []any
		want  int
	}
	var checks []check
	if len(data.conversations) > 0 {
		ids := make([]any, 0, len(data.conversations))
		for _, c := range data.conversations {
			ids = append(ids, c.ID)
		}
		checks = append(checks, check{"sessions", inClause("SELECT COUNT(*) FROM sessions WHERE id IN", ids), ids, len(data.conversations)})
		wantMsgs := 0
		for _, c := range data.conversations {
			wantMsgs += len(c.Messages)
		}
		runIDs := make([]any, 0, len(data.conversations))
		for _, c := range data.conversations {
			runIDs = append(runIDs, "legacy-"+c.ID)
		}
		checks = append(checks, check{"messages", inClause("SELECT COUNT(*) FROM messages WHERE run_id IN", runIDs), runIDs, wantMsgs})
	}
	if len(data.skills)+len(data.importedSkills) > 0 {
		ids := make([]any, 0, len(data.skills)+len(data.importedSkills))
		for _, s := range data.skills {
			ids = append(ids, s.ID)
		}
		for _, s := range data.importedSkills {
			ids = append(ids, s.ID)
		}
		checks = append(checks, check{"skills", inClause("SELECT COUNT(*) FROM skills WHERE id IN", ids), ids, len(ids)})
	}
	if len(data.mcpServers) > 0 {
		ids := make([]any, 0, len(data.mcpServers))
		for _, s := range data.mcpServers {
			ids = append(ids, s.ID)
		}
		checks = append(checks, check{"mcp_servers", inClause("SELECT COUNT(*) FROM mcp_servers WHERE id IN", ids), ids, len(ids)})
	}
	if len(data.experts) > 0 {
		ids := make([]any, 0, len(data.experts))
		for _, e := range data.experts {
			ids = append(ids, e.ID)
		}
		checks = append(checks, check{"experts", inClause("SELECT COUNT(*) FROM experts WHERE id IN", ids), ids, len(ids)})
	}
	if len(data.knowledge) > 0 {
		ids := make([]any, 0, len(data.knowledge))
		for _, k := range data.knowledge {
			ids = append(ids, k.Entry.ID)
		}
		checks = append(checks, check{"knowledge_entries", inClause("SELECT COUNT(*) FROM knowledge_entries WHERE id IN", ids), ids, len(ids)})
	}
	if len(data.settings) > 0 {
		checks = append(checks, check{"kv.settings", "SELECT COUNT(*) FROM kv WHERE key='settings'", nil, 1})
	}
	if len(data.memory) > 0 {
		keys := make([]any, 0, len(data.memory))
		for _, m := range data.memory {
			keys = append(keys, "memory:"+m.Mode)
		}
		checks = append(checks, check{"kv.memory", inClause("SELECT COUNT(*) FROM kv WHERE key IN", keys), keys, len(data.memory)})
	}
	if len(data.themes) > 0 {
		checks = append(checks, check{"kv.themes", "SELECT COUNT(*) FROM kv WHERE key LIKE 'theme:%'", nil, len(data.themes)})
	}
	if len(data.assets) > 0 {
		checks = append(checks, check{"kv.assets",
			`SELECT COUNT(*) FROM kv WHERE key LIKE 'background:%' OR key LIKE 'design_style:%'
			   OR key LIKE 'ui_component:%' OR key LIKE 'ui_components:%'`, nil, len(data.assets)})
	}

	for _, c := range checks {
		got, err := countRows(ctx, tx, c.query, c.args...)
		if err != nil {
			return fmt.Errorf("importer: verify %s: %w", c.table, err)
		}
		if got != c.want {
			return fmt.Errorf("importer: verify %s: got %d rows, want %d (整体回滚，旧数据未动)", c.table, got, c.want)
		}
		rep.Verified[c.table] = true
	}
	return nil
}

// ---------------------------------------------------------------- 小工具

func setKVInTx(ctx context.Context, tx storage.Tx, key string, value []byte, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now); err != nil {
		return fmt.Errorf("importer: kv %s: %w", key, err)
	}
	return nil
}

func inClause(prefix string, ids []any) string {
	q := prefix + " ("
	for i := range ids {
		if i > 0 {
			q += ","
		}
		q += "?"
	}
	return q + ")"
}

func checksumFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return checksumBytes(raw)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefault64(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

func orDefaultJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("[]")
	}
	return raw
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
