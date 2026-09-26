package formats

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// jsonCodec decodes with UseNumber so that a value written by the user (a large
// integer, 1.0, 1e10) is stored back exactly as it was read instead of going
// through float64.
type jsonCodec struct{}

func (jsonCodec) parse(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, jsonError(raw, err)
	}
	rest := raw[dec.InputOffset():]
	if trimmed := bytes.TrimLeft(rest, " \t\r\n"); len(trimmed) > 0 {
		at := dec.InputOffset() + int64(len(rest)-len(trimmed))
		return nil, fmt.Errorf("json: line %d: unexpected content after the top-level object",
			lineAt(raw, at))
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func (jsonCodec) render(doc map[string]any, _ []byte) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// Without this, "&" in a gateway URL would be written as \u0026.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return buf.Bytes(), nil
}

func jsonError(raw []byte, err error) error {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("json: line %d: %s", lineAt(raw, syntaxErr.Offset), syntaxErr.Error())
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Errorf("json: line %d: %s", lineAt(raw, typeErr.Offset), typeErr.Error())
	}
	return fmt.Errorf("json: %w", err)
}

// lineAt turns a byte offset into a 1-based line number.
func lineAt(raw []byte, offset int64) int {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(raw)) {
		offset = int64(len(raw))
	}
	return 1 + bytes.Count(raw[:offset], []byte("\n"))
}
