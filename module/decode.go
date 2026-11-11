package module

import (
	"encoding"
	"fmt"
	"io/fs"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Decode fills the struct dst points at from args, as a YAML decoder
// gives them (maps, lists, strings, bools, numbers).
//
// A field's key is its yaml tag's name, else its name in lower case; a
// field tagged "-" and an unexported one take no key. An embedded struct
// with no tag has its fields' keys in the struct that embeds it. A key no
// field takes is an error naming it, with the keys that lead to it
// ("repo.key_url", "files[2].mode").
//
// Values convert only where nothing is lost: a number to an integer field
// when it is a whole number in range, an integer to a float. A string
// goes to a field whose type implements encoding.TextUnmarshaler, to a
// time.Duration (time.ParseDuration), and to an fs.FileMode as octal
// ("0644", "0o644", "644"). A number for an fs.FileMode is an error: a
// YAML decoder reads 644 as the decimal number 644 and 0644 as 420, and
// the number does not say which was written. A value of type fs.FileMode
// goes to one as it is. (A playbook passes a plain 0644 or 0o644 to a
// module as the string it is.) A nil value leaves the field as it is.
func Decode(args map[string]any, dst any) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("module: Decode needs a pointer to a struct, not %T", dst)
	}
	if args == nil {
		return nil
	}
	return decodeStruct("", args, v.Elem())
}

var (
	durationType  = reflect.TypeFor[time.Duration]()
	fileModeType  = reflect.TypeFor[fs.FileMode]()
	unmarshalType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// keyPath is the key k under the path p, for errors.
func keyPath(p, k string) string {
	if p == "" {
		return k
	}
	return p + "." + k
}

// fields are the settable fields of struct v by key, embedded structs'
// included.
func fields(v reflect.Value, into map[string]reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		tag, embed, ok := fieldKey(t.Field(i))
		switch {
		case !ok:
			continue
		case embed:
			fv := v.Field(i)
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					if !fv.CanSet() {
						continue
					}
					fv.Set(reflect.New(fv.Type().Elem()))
				}
				fv = fv.Elem()
			}
			fields(fv, into)
			continue
		}
		into[tag] = v.Field(i)
	}
}

func decodeStruct(path string, m map[string]any, v reflect.Value) error {
	byKey := map[string]reflect.Value{}
	fields(v, byKey)
	for k, val := range m {
		f, ok := byKey[k]
		if !ok {
			return fmt.Errorf("unknown key %q", keyPath(path, k))
		}
		if err := decodeValue(keyPath(path, k), val, f); err != nil {
			return err
		}
	}
	return nil
}

// stringMap is m as a map with string keys: a YAML decoder may give
// map[any]any.
func stringMap(path string, m any) (map[string]any, error) {
	switch m := m.(type) {
	case map[string]any:
		return m, nil
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			s, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("%s: key %v is not a string", path, k)
			}
			out[s] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: want a map, got %s", path, describe(m))
}

func describe(x any) string {
	switch x.(type) {
	case string:
		return "a string"
	case bool:
		return "a bool"
	case []any:
		return "a list"
	case map[string]any, map[any]any:
		return "a map"
	}
	if isNumber(x) {
		return "a number"
	}
	return fmt.Sprintf("%T", x)
}

func isNumber(x any) bool {
	switch x.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	}
	return false
}

func decodeValue(path string, x any, v reflect.Value) error {
	if x == nil {
		return nil
	}
	want := func(what string) error { return fmt.Errorf("%s: want %s, got %s", path, what, describe(x)) }
	t := v.Type()

	if s, ok := x.(string); ok && reflect.PointerTo(t).Implements(unmarshalType) {
		if err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	}
	switch t {
	case durationType:
		s, ok := x.(string)
		if !ok {
			return want("a duration (\"30s\")")
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		v.SetInt(int64(d))
		return nil
	case fileModeType:
		if s, ok := x.(string); ok {
			n, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(s, "0o"), "0O"), 8, 32)
			if err != nil {
				return fmt.Errorf("%s: %q is not an octal mode", path, s)
			}
			v.SetUint(n)
			return nil
		}
		if m, ok := x.(fs.FileMode); ok {
			v.SetUint(uint64(m))
			return nil
		}
		if isNumber(x) {
			return fmt.Errorf("%s: %v is a number, and a mode is octal: write it as \"0644\" (a number written 644 is decimal 644, 0o1204)", path, x)
		}
		return want("an octal mode (\"0644\")")
	}

	switch t.Kind() {
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		if err := decodeValue(path, x, p.Elem()); err != nil {
			return err
		}
		v.Set(p)
	case reflect.Interface:
		if !reflect.TypeOf(x).AssignableTo(t) {
			return want(t.String())
		}
		v.Set(reflect.ValueOf(x))
	case reflect.String:
		// A whole number is its decimal text (port: 22). A fraction is
		// refused: YAML has already lost how it was written (1.20 is 1.2).
		if n, ok := toInt(x); ok {
			v.SetString(strconv.FormatInt(n, 10))
			return nil
		}
		s, ok := x.(string)
		if !ok {
			return want("a string")
		}
		v.SetString(s)
	case reflect.Bool:
		b, ok := x.(bool)
		if !ok {
			return want("true or false")
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := toInt(x)
		if !ok || v.OverflowInt(n) {
			return want("a whole number in range")
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u, ok := x.(uint64)
		if !ok {
			var n int64
			n, ok = toInt(x)
			ok, u = ok && n >= 0, uint64(n)
		}
		if !ok || v.OverflowUint(u) {
			return want("a whole number in range")
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		f, ok := toFloat(x)
		if !ok || v.OverflowFloat(f) {
			return want("a number")
		}
		v.SetFloat(f)
	case reflect.Slice:
		list, ok := x.([]any)
		if !ok {
			return want("a list")
		}
		s := reflect.MakeSlice(t, len(list), len(list))
		for i, e := range list {
			if err := decodeValue(fmt.Sprintf("%s[%d]", path, i), e, s.Index(i)); err != nil {
				return err
			}
		}
		v.Set(s)
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return fmt.Errorf("%s: a map field needs string keys", path)
		}
		m, err := stringMap(path, x)
		if err != nil {
			return err
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for k, e := range m {
			ev := reflect.New(t.Elem()).Elem()
			if err := decodeValue(keyPath(path, k), e, ev); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), ev)
		}
		v.Set(out)
	case reflect.Struct:
		m, err := stringMap(path, x)
		if err != nil {
			return err
		}
		return decodeStruct(path, m, v)
	default:
		return fmt.Errorf("%s: a field of type %s cannot be decoded", path, t)
	}
	return nil
}

// toInt is x as an int64, when it is a whole number that fits.
func toInt(x any) (int64, bool) {
	switch n := x.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return toInt(uint64(n))
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), n <= math.MaxInt64
	case float32:
		return toInt(float64(n))
	case float64:
		if n != math.Trunc(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}

func toFloat(x any) (float64, bool) {
	switch n := x.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	if i, ok := toInt(x); ok {
		return float64(i), true
	}
	if u, ok := x.(uint64); ok {
		return float64(u), true
	}
	return 0, false
}
