package secrets

import (
	"reflect"
	"strings"
)

// redactReflect 对 struct 等复合类型做反射脱敏：导出字符串字段逐一过滤，
// 字段名命中敏感模式时整体替换。不可寻址/不可导出的字段安全跳过。
func (r *Redactor) redactReflect(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	redacted := r.redactReflectValue(rv)
	if !redacted.IsValid() {
		return v
	}
	return redacted.Interface()
}

func (r *Redactor) redactReflectValue(rv reflect.Value) reflect.Value {
	switch rv.Kind() {
	case reflect.Invalid:
		return rv
	case reflect.String:
		out := reflect.New(rv.Type()).Elem()
		out.SetString(r.RedactString(rv.String()))
		return out
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return rv
		}
		elem := r.redactReflectValue(rv.Elem())
		if !elem.IsValid() {
			return rv
		}
		if rv.Kind() == reflect.Pointer {
			out := reflect.New(rv.Type().Elem())
			out.Elem().Set(elem)
			return out
		}
		out := reflect.New(rv.Type()).Elem()
		out.Set(elem)
		return out
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return rv
		}
		// []byte / json.RawMessage：按字符串脱敏后转回字节切片
		// （审核报告 S-1：这类形态以前会被逐字节处理而漏掉内容）。
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			out := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
			for i := 0; i < rv.Len(); i++ {
				out.Index(i).SetUint(rv.Index(i).Uint())
			}
			redacted := r.RedactString(string(out.Bytes()))
			bytes := reflect.MakeSlice(rv.Type(), len(redacted), len(redacted))
			for i := 0; i < len(redacted); i++ {
				bytes.Index(i).SetUint(uint64(redacted[i]))
			}
			return bytes
		}
		out := reflect.MakeSlice(reflect.SliceOf(rv.Type().Elem()), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			elem := r.redactReflectValue(rv.Index(i))
			if elem.IsValid() {
				out.Index(i).Set(elem)
			}
		}
		return out
	case reflect.Map:
		if rv.IsNil() {
			return rv
		}
		out := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := iter.Key()
			val := r.redactReflectValue(iter.Value())
			if key.Kind() == reflect.String && sensitiveKeyPattern.MatchString(key.String()) {
				placeholder := reflect.New(rv.Type().Elem()).Elem()
				if placeholder.Kind() == reflect.String {
					placeholder.SetString(redactedPlaceholder)
				}
				out.SetMapIndex(key, placeholder)
				continue
			}
			if val.IsValid() {
				out.SetMapIndex(key, val)
			} else {
				out.SetMapIndex(key, iter.Value())
			}
		}
		return out
	case reflect.Struct:
		out := reflect.New(rv.Type()).Elem()
		out.Set(rv)
		t := rv.Type()
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue // 未导出字段
			}
			fv := out.Field(i)
			if sensitiveKeyPattern.MatchString(field.Name) {
				if fv.Kind() == reflect.String && fv.CanSet() {
					fv.SetString(redactedPlaceholder)
				}
				continue
			}
			redactedField := r.redactReflectValue(fv)
			if redactedField.IsValid() && fv.CanSet() {
				fv.Set(redactedField)
			}
		}
		return out
	default:
		return rv
	}
}

// SanitizeForLog 是给日志路径使用的便捷脱敏：去除换行后做字符串级过滤，
// 防止多行 secret 通过日志注入绕过。
func (r *Redactor) SanitizeForLog(s string) string {
	return r.RedactString(strings.ReplaceAll(s, "\n", "\\n"))
}
