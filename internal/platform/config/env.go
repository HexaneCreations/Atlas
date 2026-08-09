package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// durationType is compared by identity because time.Duration has kind Int64
// and would otherwise be parsed as a plain integer.
var durationType = reflect.TypeOf(time.Duration(0))

// LookupFunc reads an environment variable. Injected so tests can supply an
// environment without mutating the process's.
type LookupFunc func(key string) (string, bool)

// ReadFileFunc reads a file. Injected for the same reason.
type ReadFileFunc func(path string) ([]byte, error)

// bindEnv overlays environment variables onto the struct pointed to by target.
//
// Variable names are derived from the `env` struct tags along the path to each
// field, joined with underscores beneath prefix: the Level field of the
// Logging section becomes ATLAS_LOGGING_LEVEL. Deriving names from the shape
// of the config struct rather than a hand-written table means a new field
// cannot be added without its environment override existing too.
//
// Fields whose variable is unset are left untouched, which is what makes the
// precedence chain (defaults, then file, then environment) work: binding is
// an overlay, never a reset.
func bindEnv(target any, prefix string, lookup LookupFunc, readFile ReadFileFunc) error {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("bindEnv: target must be a non-nil pointer to struct, got %T", target)
	}

	// A setting whose own name ends in _FILE — node.id_file, say — would
	// otherwise be swallowed by the secret-file convention below: ATLAS_NODE_ID
	// plus the _FILE suffix is exactly ATLAS_NODE_ID_FILE. Collecting the
	// declared names first lets an explicitly declared variable win over the
	// indirection, so both conventions can coexist.
	declared := make(map[string]bool)
	for _, name := range envNames(target, prefix) {
		declared[name] = true
	}
	return bindStruct(v.Elem(), prefix, lookup, readFile, declared)
}

func bindStruct(v reflect.Value, prefix string, lookup LookupFunc, readFile ReadFileFunc, declared map[string]bool) error {
	t := v.Type()
	for i := range t.NumField() {
		field, value := t.Field(i), v.Field(i)
		if !field.IsExported() {
			continue
		}
		tag, ok := field.Tag.Lookup("env")
		if !ok || tag == "-" {
			continue
		}
		name := prefix + "_" + tag

		if value.Kind() == reflect.Struct && value.Type() != durationType {
			if err := bindStruct(value, name, lookup, readFile, declared); err != nil {
				return err
			}
			continue
		}

		raw, found, err := resolve(name, value.Kind(), lookup, readFile, declared)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := setValue(value, raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// FileSuffix is appended to an environment variable name to source a string
// value from a file instead. ATLAS_DATABASE_PASSWORD_FILE=/run/secrets/db is
// how Atlas consumes Docker and Kubernetes secrets, which are mounted as
// files precisely so they never appear in a process's environment — where
// anything that can read /proc could pick them up.
const FileSuffix = "_FILE"

func resolve(name string, kind reflect.Kind, lookup LookupFunc, readFile ReadFileFunc, declared map[string]bool) (string, bool, error) {
	// Skip the indirection when the derived name is itself a setting.
	if kind == reflect.String && !declared[name+FileSuffix] {
		if path, ok := lookup(name + FileSuffix); ok && path != "" {
			b, err := readFile(path)
			if err != nil {
				return "", false, fmt.Errorf("%s%s: read secret file: %w", name, FileSuffix, err)
			}
			// Trailing newlines are near-universal in mounted secrets and
			// are never part of the credential.
			return strings.TrimRight(string(b), "\r\n"), true, nil
		}
	}
	raw, ok := lookup(name)
	return raw, ok, nil
}

func setValue(v reflect.Value, raw string) error {
	if v.Type() == durationType {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid duration %q (want e.g. 30s, 5m)", raw)
		}
		v.SetInt(int64(d))
		return nil
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid boolean %q (want true or false)", raw)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid unsigned integer %q", raw)
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid number %q", raw)
		}
		v.SetFloat(f)
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element type %s", v.Type().Elem())
		}
		v.Set(reflect.ValueOf(splitList(raw)))
	default:
		return fmt.Errorf("unsupported field type %s", v.Type())
	}
	return nil
}

// splitList parses a comma-separated environment value. An empty or
// whitespace-only value yields an empty, non-nil slice, so that setting a list
// variable to "" explicitly clears a default rather than silently keeping it.
func splitList(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envNames walks a config struct and returns every environment variable it
// recognises, in declaration order. It backs `atlas-server config --env` and
// the generated configuration reference, so documentation cannot drift from
// the struct.
func envNames(target any, prefix string) []string {
	var out []string
	var walk func(v reflect.Value, prefix string)
	walk = func(v reflect.Value, prefix string) {
		t := v.Type()
		for i := range t.NumField() {
			field, value := t.Field(i), v.Field(i)
			tag, ok := field.Tag.Lookup("env")
			if !field.IsExported() || !ok || tag == "-" {
				continue
			}
			name := prefix + "_" + tag
			if value.Kind() == reflect.Struct && value.Type() != durationType {
				walk(value, name)
				continue
			}
			out = append(out, name)
		}
	}
	walk(reflect.ValueOf(target).Elem(), prefix)
	return out
}
