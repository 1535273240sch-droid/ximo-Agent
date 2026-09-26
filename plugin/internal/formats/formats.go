// Package formats reads and writes the configuration file formats used by
// ximo-plugin adapter specs: json, toml, yaml and dotenv.
//
// # Guarantees
//
// Read never fails because the file is missing: it returns an empty document
// and a nil error, so the "should I create it?" decision stays with the caller.
// A file that exists but does not parse produces an error carrying the path and
// a line number -- it is never silently ignored.
//
// Write is read-modify-write, not a blind overwrite. The document handed in is
// merged into what the file already contains, so keys the caller knows nothing
// about survive. The merge is deep, and index-wise for arrays: setting
// "models.0.api_base" keeps models[1] and every sibling key of models[0]. Write
// always stores the file and always takes its <path>.bak-<unix> backup when
// asked to; only an empty document is a no-op, since there is nothing to apply.
//
// Write is atomic. Bytes go to a temporary file in the same directory, which is
// then renamed over the target, so a failed write never leaves a half-written
// file. backup=true first copies the previous bytes to <path>.bak-<unix>.
//
// The original line ending style survives: a CRLF file is written back as CRLF.
// New files are written with LF and mode 0600; existing files keep their mode.
// A leading UTF-8 BOM is preserved.
//
// # Formatting, comments and key order
//
// dotenv and yaml are patched in place, so comments, blank lines, key order and
// the original quoting style survive: only the values that actually differ are
// rewritten. A yaml file that has to change is re-emitted with two-space
// indentation (values, comments and key order are kept; the indentation width is
// not). json and toml are re-encoded from the parsed document, so their
// formatting (indentation, key order, comments) is normalised while every
// key/value is kept. Diff renders both sides through the same encoder, so that
// normalisation cancels out and only real value changes show up.
package formats

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Format names accepted by Read, Write and Diff.
const (
	FormatJSON   = "json"
	FormatTOML   = "toml"
	FormatYAML   = "yaml"
	FormatDotenv = "dotenv"
)

// Formats returns the canonical format names, in a stable order.
func Formats() []string {
	return []string{FormatJSON, FormatTOML, FormatYAML, FormatDotenv}
}

// Normalize maps a spec's format field onto a canonical format name. It accepts
// the two common aliases "yml" and "env".
func Normalize(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatJSON:
		return FormatJSON, nil
	case FormatTOML:
		return FormatTOML, nil
	case FormatYAML, "yml":
		return FormatYAML, nil
	case FormatDotenv, "env":
		return FormatDotenv, nil
	}
	return "", fmt.Errorf("formats: unsupported format %q (want json, toml, yaml or dotenv)", format)
}

// codec is implemented once per format. parse turns file bytes into a document;
// render turns a document back into the exact bytes Write would store, using
// the original bytes for the formats that patch rather than re-encode.
type codec interface {
	parse(raw []byte) (map[string]any, error)
	render(doc map[string]any, raw []byte) ([]byte, error)
}

func codecFor(format string) (codec, error) {
	name, err := Normalize(format)
	if err != nil {
		return nil, err
	}
	switch name {
	case FormatJSON:
		return jsonCodec{}, nil
	case FormatTOML:
		return tomlCodec{}, nil
	case FormatYAML:
		return yamlCodec{}, nil
	default:
		return dotenvCodec{}, nil
	}
}

// parseDoc parses body and normalises the result, so that a document from any
// codec uses the same container types (map[string]any and []any). The toml
// decoder, for one, produces []map[string]any for arrays of tables.
func parseDoc(c codec, body []byte) (map[string]any, error) {
	doc, err := c.parse(body)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	return normalizeDoc(doc), nil
}

// Read parses path as format. A missing file yields an empty document and a nil
// error; a file that exists but cannot be parsed yields an error naming the
// file and the offending line.
func Read(path, format string) (map[string]any, error) {
	c, err := codecFor(format)
	if err != nil {
		return nil, err
	}
	st, err := load(path)
	if err != nil {
		return nil, err
	}
	if !st.exists {
		return map[string]any{}, nil
	}
	doc, err := parseDoc(c, st.body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

// Write merges doc into the file at path, preserving unknown keys and the
// file's line endings, and replaces the file atomically. When backup is true
// the previous content is first copied to <path>.bak-<unix seconds>; two writes
// in the same second therefore share one backup file, which is what the
// contract's name pattern implies. The document's containers are normalised in
// place to map[string]any / []any.
func Write(path, format string, doc map[string]any, backup bool) error {
	c, err := codecFor(format)
	if err != nil {
		return err
	}
	if len(doc) == 0 {
		return nil
	}
	// Normalise the caller's containers too, so an array built by another
	// decoder still merges index-wise instead of replacing the file's array.
	doc = normalizeDoc(doc)
	st, err := load(path)
	if err != nil {
		return err
	}
	current := map[string]any{}
	if st.exists {
		current, err = parseDoc(c, st.body)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	out, err := st.render(c, mergeDocs(current, doc))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if backup && st.exists {
		bak := backupName(path, time.Now())
		if err := os.WriteFile(bak, st.original, st.perm); err != nil {
			return fmt.Errorf("formats: backup %s: %w", bak, err)
		}
	}
	return writeAtomic(path, out, st.perm)
}

// Diff renders the current file and the file as it would look after
// io.Write(path, format, next, ...) as a unified-style text diff. It never
// writes. A missing file is diffed against an empty document, and no change at
// all yields an empty string.
func Diff(path, format string, next map[string]any) (string, error) {
	c, err := codecFor(format)
	if err != nil {
		return "", err
	}
	if len(next) == 0 {
		return "", nil
	}
	next = normalizeDoc(next)
	st, err := load(path)
	if err != nil {
		return "", err
	}
	current := map[string]any{}
	if st.exists {
		current, err = parseDoc(c, st.body)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
	}
	before, err := st.render(c, current)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	after, err := st.render(c, mergeDocs(current, next))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if !st.exists {
		before = nil
	}
	return unifiedDiff(path, string(before), string(after)), nil
}

// mergeDocs merges src into dst and returns dst. Nested documents are merged
// key-wise, arrays are merged index-wise, and anything else is replaced by the
// incoming value. dst is modified in place.
func mergeDocs(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if cur, ok := dst[k]; ok {
			if merged, ok := mergeValues(cur, v); ok {
				dst[k] = merged
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

func mergeValues(current, next any) (any, bool) {
	switch cur := current.(type) {
	case map[string]any:
		if in, ok := next.(map[string]any); ok {
			return mergeDocs(cur, in), true
		}
	case []any:
		if in, ok := next.([]any); ok {
			for i, v := range in {
				if i >= len(cur) {
					cur = append(cur, v)
					continue
				}
				if merged, ok := mergeValues(cur[i], v); ok {
					cur[i] = merged
				} else {
					cur[i] = v
				}
			}
			return cur, true
		}
	}
	return nil, false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalizeDoc rewrites the containers of a parsed document into map[string]any
// and []any, in place.
func normalizeDoc(doc map[string]any) map[string]any {
	for k, v := range doc {
		doc[k] = normalizeValue(v)
	}
	return doc
}

func normalizeValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return normalizeDoc(v)
	case []any:
		for i, elem := range v {
			v[i] = normalizeValue(elem)
		}
		return v
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = normalizeValue(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return value
		}
		out := make(map[string]any, rv.Len())
		for _, key := range rv.MapKeys() {
			out[key.String()] = normalizeValue(rv.MapIndex(key).Interface())
		}
		return out
	}
	return value
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// fileState is everything Read, Write and Diff need to know about the file on
// disk. body is normalised to LF with any BOM removed; original keeps the exact
// bytes as read, for backups.
type fileState struct {
	exists   bool
	original []byte
	body     []byte
	bom      bool
	crlf     bool
	perm     os.FileMode
}

func load(path string) (fileState, error) {
	st := fileState{perm: 0o600}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("formats: read %s: %w", path, err)
	}
	st.exists = true
	st.original = raw
	st.crlf = bytes.Contains(raw, []byte("\r\n"))
	st.bom = bytes.HasPrefix(raw, utf8BOM)
	st.body = normalizeEOL(raw)
	if st.bom {
		st.body = st.body[len(utf8BOM):]
	}
	if fi, err := os.Stat(path); err == nil {
		st.perm = fi.Mode().Perm()
	}
	return st, nil
}

// render encodes doc the way it would be stored: with the original line ending
// style, BOM and nothing else changed about the file's environment.
func (st fileState) render(c codec, doc map[string]any) ([]byte, error) {
	out, err := c.render(doc, st.body)
	if err != nil {
		return nil, err
	}
	if st.crlf {
		out = toCRLF(out)
	}
	if st.bom {
		out = append(append([]byte{}, utf8BOM...), out...)
	}
	return out, nil
}

func normalizeEOL(raw []byte) []byte {
	return bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
}

func toCRLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}

func backupName(path string, now time.Time) string {
	return fmt.Sprintf("%s.bak-%d", path, now.Unix())
}

// writeAtomic writes data to a temporary file next to path and renames it into
// place, so path either keeps its previous content or holds all of data.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".ximo-plugin-*.tmp")
	if err != nil {
		return fmt.Errorf("formats: create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	done := false
	defer func() {
		if !done {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm.Perm()); err != nil {
		return fmt.Errorf("formats: chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("formats: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("formats: sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("formats: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("formats: replace %s: %w", path, err)
	}
	done = true
	return nil
}
