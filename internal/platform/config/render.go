package config

import (
	"reflect"
	"time"
)

// Render converts the configuration into a plain map suitable for display.
//
// It exists because time.Duration marshals to JSON as an integer count of
// nanoseconds: `atlas-server config` would report a thirty-second timeout as
// 30000000000. That is unambiguous and useless — the command's whole purpose
// is to let an operator confirm, at a glance, that a value is what they
// intended, and comparing nanosecond counts against a YAML file written in
// "30s" defeats it.
//
// Durations are rendered with their String form, which is exactly the syntax
// accepted in configuration, so the output can be pasted straight back into a
// file or an environment variable.
//
// The transform is driven by the json tags already on the config structs, so
// a new field appears here without anything being added.
func (c *Config) Render() map[string]any {
	out, _ := renderValue(reflect.ValueOf(*c)).(map[string]any)
	return out
}

func renderValue(v reflect.Value) any {
	if v.Type() == durationType {
		return time.Duration(v.Int()).String()
	}

	switch v.Kind() {
	case reflect.Struct:
		return renderStruct(v)

	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return []any{}
		}
		items := make([]any, 0, v.Len())
		for i := range v.Len() {
			items = append(items, renderValue(v.Index(i)))
		}
		return items

	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return renderValue(v.Elem())

	default:
		return v.Interface()
	}
}

func renderStruct(v reflect.Value) map[string]any {
	out := make(map[string]any)
	t := v.Type()

	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		// A json tag of "-" marks a field that must never be displayed — the
		// database password, above all. Honouring the tag here means the
		// redaction rule lives in exactly one place.
		name, ok := field.Tag.Lookup("json")
		if !ok || name == "-" {
			continue
		}
		out[name] = renderValue(v.Field(i))
	}
	return out
}
