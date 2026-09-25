package tool

import (
	"fmt"
	"strings"
)

// ValidateArguments 按工具定义的 JSON Schema 子集校验调用参数。
// 支持的子集：type(boolean/number/string/object/array)、properties、required、
// enum、items、additionalProperties。default 不参与校验（由调用方填充）。
//
// 校验失败返回的 error 消息面向 LLM/UI 可读，会原样进入 ToolResponse.Error。
func ValidateArguments(schema JSONSchema, args map[string]any) error {
	if schema.Type != "object" {
		// 非 object 顶层 schema 的工具极少；宽松放行，由工具自身校验。
		return nil
	}
	var problems []string

	for _, req := range schema.Required {
		if _, ok := args[req]; !ok {
			problems = append(problems, fmt.Sprintf("缺少必填参数 %q", req))
		}
	}

	for name, prop := range schema.Properties {
		val, ok := args[name]
		if !ok {
			continue
		}
		if err := validateValue(prop, val); err != nil {
			problems = append(problems, fmt.Sprintf("参数 %q: %v", name, err))
		}
	}

	if schema.AdditionalProperties != nil {
		for name, val := range args {
			if _, declared := schema.Properties[name]; declared {
				continue
			}
			if err := validateValue(*schema.AdditionalProperties, val); err != nil {
				problems = append(problems, fmt.Sprintf("参数 %q: %v", name, err))
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("schema 校验失败: %s", strings.Join(problems, "; "))
	}
	return nil
}

func validateValue(schema JSONSchema, val any) error {
	switch schema.Type {
	case "", "any":
		return nil
	case "object":
		m, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("期望 object，实际 %s", jsonTypeOf(val))
		}
		for name, prop := range schema.Properties {
			if sub, exists := m[name]; exists {
				if err := validateValue(prop, sub); err != nil {
					return fmt.Errorf("字段 %q: %v", name, err)
				}
			}
		}
		for _, req := range schema.Required {
			if _, exists := m[req]; !exists {
				return fmt.Errorf("缺少必填字段 %q", req)
			}
		}
		return nil
	case "array":
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("期望 array，实际 %s", jsonTypeOf(val))
		}
		if schema.Items != nil {
			for i, item := range arr {
				if err := validateValue(*schema.Items, item); err != nil {
					return fmt.Errorf("元素[%d]: %v", i, err)
				}
			}
		}
		return nil
	case "string":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("期望 string，实际 %s", jsonTypeOf(val))
		}
		return checkEnum(schema, s)
	case "number":
		n, ok := toFloat(val)
		if !ok {
			return fmt.Errorf("期望 number，实际 %s", jsonTypeOf(val))
		}
		return checkEnum(schema, n)
	case "boolean":
		b, ok := val.(bool)
		if !ok {
			return fmt.Errorf("期望 boolean，实际 %s", jsonTypeOf(val))
		}
		return checkEnum(schema, b)
	case "integer":
		n, ok := toFloat(val)
		if !ok {
			return fmt.Errorf("期望 integer，实际 %s", jsonTypeOf(val))
		}
		if n != float64(int64(n)) {
			return fmt.Errorf("期望 integer，实际 %v", n)
		}
		return checkEnum(schema, n)
	default:
		return fmt.Errorf("未知 schema 类型 %q", schema.Type)
	}
}

func checkEnum(schema JSONSchema, val any) error {
	if len(schema.Enum) == 0 {
		return nil
	}
	for _, allowed := range schema.Enum {
		if valuesEqual(allowed, val) {
			return nil
		}
	}
	return fmt.Errorf("取值 %v 不在允许集合 %v 内", val, schema.Enum)
}

func valuesEqual(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			return fa == fb
		}
		return false
	}
	return a == b
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	default:
		return 0, false
	}
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		if _, ok := toFloat(v); ok {
			return "number"
		}
		return fmt.Sprintf("%T", v)
	}
}
