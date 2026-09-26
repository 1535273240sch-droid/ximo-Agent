package formats

import (
	"bytes"
	"fmt"

	"github.com/BurntSushi/toml"
)

// tomlCodec re-encodes the document. BurntSushi/toml exposes no AST, so toml
// comments and key order are not preserved -- see the package doc.
type tomlCodec struct{}

func (tomlCodec) parse(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		// toml.ParseError.Error() already reads "toml: line N: ...".
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func (tomlCodec) render(doc map[string]any, _ []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return nil, fmt.Errorf("toml: %w", err)
	}
	return buf.Bytes(), nil
}
