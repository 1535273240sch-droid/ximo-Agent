package formats

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxAutoIndex bounds how far SetPath will grow an array. A spec that asks for
// "models.9000.x" is a bug, not an intention, and should fail loudly instead of
// allocating thousands of nil placeholders.
const maxAutoIndex = 1024

// SetPath writes value at a dotted path inside doc, creating missing levels.
// A segment made of digits is an array index, so "models.0.api_base" addresses
// the api_base key of the first element of models. A nil level becomes a map,
// or a slice when the next segment is an index. Trying to descend through a
// value that is neither (for example setting "a.b" when a is a string) is an
// error rather than a silent overwrite.
func SetPath(doc map[string]any, dotPath string, value any) error {
	if doc == nil {
		return errors.New("formats: SetPath needs a non-nil document")
	}
	segments, err := splitDotPath(dotPath)
	if err != nil {
		return err
	}
	_, err = assignPath(doc, segments, value, dotPath, documentRoot)
	return err
}

func splitDotPath(dotPath string) ([]string, error) {
	if strings.TrimSpace(dotPath) == "" {
		return nil, errors.New("formats: empty path")
	}
	segments := strings.Split(dotPath, ".")
	for _, s := range segments {
		if s == "" {
			return nil, fmt.Errorf("formats: path %q has an empty segment", dotPath)
		}
	}
	return segments, nil
}

// assignPath returns the value that should be stored in place of node. at names
// the node inside the document, so a conflict can point at what actually blocks
// the path rather than at the segment the caller asked for.
func assignPath(node any, segments []string, value any, full, at string) (any, error) {
	if len(segments) == 0 {
		return value, nil
	}
	segment := segments[0]
	if index, isIndex := arrayIndex(segment); isIndex {
		var array []any
		switch current := node.(type) {
		case nil:
		case []any:
			array = current
		default:
			return nil, fmt.Errorf("formats: cannot set %q: %s is %s, not an array", full, at, typeName(node))
		}
		if index >= maxAutoIndex {
			return nil, fmt.Errorf("formats: cannot set %q: index %d is out of range (limit %d)", full, index, maxAutoIndex)
		}
		for len(array) <= index {
			array = append(array, nil)
		}
		childAt := strconv.Quote(segment)
		if at != documentRoot {
			childAt = at + "." + strconv.Quote(segment)
		}
		child, err := assignPath(array[index], segments[1:], value, full, childAt)
		if err != nil {
			return nil, err
		}
		array[index] = child
		return array, nil
	}

	var object map[string]any
	switch current := node.(type) {
	case nil:
		object = map[string]any{}
	case map[string]any:
		object = current
	default:
		return nil, fmt.Errorf("formats: cannot set %q: %s is %s, not an object", full, at, typeName(node))
	}
	child, err := assignPath(object[segment], segments[1:], value, full, strconv.Quote(segment))
	if err != nil {
		return nil, err
	}
	object[segment] = child
	return object, nil
}

// documentRoot names the value SetPath starts from.
const documentRoot = "the document root"

// arrayIndex reports whether a path segment is an array index. Leading zeros are
// treated as a key ("01" is a map key), which keeps "0" unambiguous.
func arrayIndex(segment string) (int, bool) {
	if segment == "" || (len(segment) > 1 && segment[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(segment); i++ {
		if segment[i] < '0' || segment[i] > '9' {
			return 0, false
		}
	}
	index, err := strconv.Atoi(segment)
	if err != nil {
		return 0, false
	}
	return index, true
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "a number"
	default:
		return fmt.Sprintf("a %T", value)
	}
}
