package module

import (
	"errors"
	"reflect"
	"strings"
)

// Arg is one argument a module takes, for a listing of modules.
type Arg struct {
	// Key is the argument's key, with the keys that lead to it for one
	// inside another ("repo.key_url", "items[].name").
	Key string
	// Type is what the value is: string, bool, int, number, duration,
	// mode, any, list of T, map of T, or object for a struct whose keys
	// follow it.
	Type string
	// Doc is the field's doc tag (`doc:"what it is"`), or "".
	Doc string
}

// errDescribe is what a Typed Ctor returns when Args asks it to describe
// its arguments instead of making a resource.
var errDescribe = errors.New("module: described")

// Args are the arguments of the module registered as name, in field
// order. ok is false when there is no such module, or its Ctor was not
// made with Typed and so says nothing of its arguments.
func (r *Registry) Args(name string) (args []Arg, ok bool) {
	ctor, found := r.Lookup(name)
	if !found {
		return nil, false
	}
	var out []Arg
	described := false
	_, err := ctor(Env{describe: func(a []Arg) { out, described = a, true }}, nil)
	if !described || !errors.Is(err, errDescribe) {
		return nil, false
	}
	return out, true
}

// argsOf are the keys Decode fills in a struct of type t.
func argsOf(prefix string, t reflect.Type, into []Arg) []Arg {
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				into = argsOf(prefix, ft, into)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if tag == "" {
			tag = strings.ToLower(f.Name)
		}
		key := prefix + tag
		into = append(into, Arg{Key: key, Type: typeName(f.Type), Doc: f.Tag.Get("doc")})
		// The keys inside a struct, or a list or map of them.
		ft, suffix := f.Type, "."
		for {
			switch ft.Kind() {
			case reflect.Pointer:
				ft = ft.Elem()
				continue
			case reflect.Slice:
				ft, suffix = ft.Elem(), suffix[:len(suffix)-1]+"[]."
				continue
			case reflect.Map:
				ft, suffix = ft.Elem(), suffix[:len(suffix)-1]+".*."
				continue
			}
			break
		}
		if ft.Kind() == reflect.Struct && !isLeaf(ft) {
			into = argsOf(key+suffix, ft, into)
		}
	}
	return into
}

// isLeaf reports whether Decode fills a value of type t from one scalar.
func isLeaf(t reflect.Type) bool {
	return t == durationType || t == fileModeType || reflect.PointerTo(t).Implements(unmarshalType) || t.Implements(unmarshalType)
}

func typeName(t reflect.Type) string {
	switch {
	case t == durationType:
		return "duration"
	case t == fileModeType:
		return "mode"
	case isLeaf(t):
		return "string"
	}
	switch t.Kind() {
	case reflect.Pointer:
		return typeName(t.Elem())
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "list of " + typeName(t.Elem())
	case reflect.Map:
		return "map of " + typeName(t.Elem())
	case reflect.Struct:
		return "object"
	}
	return "any"
}
