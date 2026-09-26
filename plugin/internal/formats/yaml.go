package formats

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// yamlCodec patches the parsed document into the original yaml.Node tree, which
// keeps key order, comments and anchors of everything the caller did not touch.
type yamlCodec struct{}

func (yamlCodec) parse(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func (yamlCodec) render(doc map[string]any, raw []byte) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if len(bytes.TrimSpace(raw)) > 0 {
		var parsed yaml.Node
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			return nil, err
		}
		node := &parsed
		if node.Kind == yaml.DocumentNode {
			if len(node.Content) == 0 {
				node = nil
			} else {
				node = node.Content[0]
			}
		}
		if node != nil {
			if node.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("yaml: document root is %s, expected a mapping", nodeKind(node.Kind))
			}
			root = node
		}
	}
	if err := mergeYAML(root, doc); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mergeYAML writes val into node, descending into mappings and sequences when
// both sides have the same shape and replacing the node in place otherwise.
func mergeYAML(node *yaml.Node, val any) error {
	switch v := val.(type) {
	case map[string]any:
		if node.Kind == yaml.MappingNode {
			for _, key := range sortedKeys(v) {
				if i := mappingIndex(node, key); i >= 0 {
					if err := mergeYAML(node.Content[i+1], v[key]); err != nil {
						return err
					}
					continue
				}
				child, err := yamlNode(v[key])
				if err != nil {
					return err
				}
				node.Content = append(node.Content, &yaml.Node{
					Kind:  yaml.ScalarNode,
					Tag:   "!!str",
					Value: key,
				}, child)
			}
			return nil
		}
	case []any:
		if node.Kind == yaml.SequenceNode {
			for i, elem := range v {
				if i < len(node.Content) {
					if err := mergeYAML(node.Content[i], elem); err != nil {
						return err
					}
					continue
				}
				child, err := yamlNode(elem)
				if err != nil {
					return err
				}
				node.Content = append(node.Content, child)
			}
			return nil
		}
	}
	replacement, err := yamlNode(val)
	if err != nil {
		return err
	}
	if valueMatchesNode(node, val) {
		// The value is already what the caller wants: leave the node (and so
		// its quoting style, comments and anchor) exactly as it was.
		return nil
	}
	// Keep the comments and the anchor that belonged to the old value, and the
	// quoting style when a string is simply being replaced by another string.
	replacement.HeadComment = node.HeadComment
	replacement.LineComment = node.LineComment
	replacement.FootComment = node.FootComment
	replacement.Anchor = node.Anchor
	if _, isString := val.(string); isString && node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		replacement.Style = node.Style
	}
	*node = *replacement
	return nil
}

// valueMatchesNode reports whether the node already holds val, so that writing
// a document back does not reformat values nobody touched.
func valueMatchesNode(node *yaml.Node, val any) bool {
	if node.Kind != yaml.ScalarNode {
		return false
	}
	var current any
	if err := node.Decode(&current); err != nil {
		return false
	}
	return sameScalar(current, val)
}

func sameScalar(current, next any) bool {
	switch c := current.(type) {
	case nil:
		return next == nil
	case string:
		s, ok := next.(string)
		return ok && s == c
	case bool:
		b, ok := next.(bool)
		return ok && b == c
	case int:
		return numberEquals(c, next)
	case int64:
		return numberEquals(c, next)
	case uint64:
		return numberEquals(c, next)
	case float64:
		return numberEquals(c, next)
	case json.Number:
		return numberEquals(c, next)
	}
	return false
}

func numberEquals(current any, next any) bool {
	toFloat := func(v any) (float64, bool) {
		switch n := v.(type) {
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		case uint64:
			return float64(n), true
		case float64:
			return n, true
		case json.Number:
			f, err := n.Float64()
			return f, err == nil
		}
		return 0, false
	}
	a, okA := toFloat(current)
	b, okB := toFloat(next)
	return okA && okB && a == b
}

func yamlNode(val any) (*yaml.Node, error) {
	encoded, err := yaml.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(encoded, &doc); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
		}
		return doc.Content[0], nil
	}
	return &doc, nil
}

// mappingIndex returns the index of key inside a mapping node's Content, or -1.
func mappingIndex(node *yaml.Node, key string) int {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Kind == yaml.ScalarNode && node.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func nodeKind(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "a document"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	}
	return "an unknown node"
}
