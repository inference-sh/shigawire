package playbook

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// EnvPrefix is the prefix of an environment variable that sets a
// playbook variable: SHIGA_VAR_DOMAIN sets ${DOMAIN}. Only prefixed
// variables are read, so the rest of the environment (USER, HOME, PATH)
// never sets one.
const EnvPrefix = "SHIGA_VAR_"

// validName reports whether s is a variable or register name: a letter or
// _, then letters, digits and _.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// resolveVars is the variables of a playbook whose defaults (its vars and
// those of the files it includes) are defaults, from lowest to highest
// precedence: defaults, the environment, each vars file in order, then
// set. defaults becomes the result.
func resolveVars(defaults map[string]string, opt Options) (map[string]string, error) {
	vars := defaults
	for _, kv := range opt.Environ {
		k, v, ok := strings.Cut(kv, "=")
		if name, found := strings.CutPrefix(k, EnvPrefix); ok && found && validName(name) {
			vars[name] = v
		}
	}
	for _, f := range opt.VarFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(f, ".env") {
			if err := readEnvFile(f, b, vars); err != nil {
				return nil, err
			}
			continue
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, yamlErr(f, err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		if err := readVars(f, doc.Content[0], vars); err != nil {
			return nil, err
		}
	}
	for k, v := range opt.Set {
		if !validName(k) {
			return nil, fmt.Errorf("variable %q: a name is letters, digits and _, not starting with a digit", k)
		}
		vars[k] = v
	}
	return vars, nil
}

// readEnvFile reads a dotenv file's NAME=VALUE lines into vars: blank
// lines and # comments are skipped, an "export " prefix is dropped, and
// one pair of matching quotes around a value is removed. Nothing is
// expanded.
func readEnvFile(file string, b []byte, vars map[string]string) error {
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		k = strings.TrimSpace(k)
		if !ok || !validName(k) {
			return fmt.Errorf("%s:%d: want NAME=VALUE", file, i+1)
		}
		vars[k] = unquote(strings.TrimSpace(v))
	}
	return nil
}

// readVars reads a mapping of names to scalars into vars. A value is
// taken as written: it is not expanded.
func readVars(file string, n *yaml.Node, vars map[string]string) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return errAt(file, n, "variables are a mapping of names to values")
	}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !validName(k.Value) {
			return errAt(file, k, "variable %q: a name is letters, digits and _, not starting with a digit", k.Value)
		}
		if v.Kind != yaml.ScalarNode {
			return errAt(file, v, "variable %s: want a single value (a string, number or bool)", k.Value)
		}
		if v.Tag == "!!null" {
			vars[k.Value] = ""
			continue
		}
		vars[k.Value] = v.Value
	}
	return nil
}

// undefinedError is a ${NAME} with no variable NAME.
type undefinedError struct{ name string }

func (e *undefinedError) Error() string { return "undefined variable ${" + e.name + "}" }

// expand replaces each ${NAME} in s with the variable's value. $${ is a
// literal ${. An undefined variable is an error.
func expand(s string, vars map[string]string) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		if i > 0 && s[i-1] == '$' {
			b.WriteString(s[:i-1])
			b.WriteString("${")
			s = s[i+2:]
			continue
		}
		b.WriteString(s[:i])
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("unclosed ${ in %q", s)
		}
		name := s[i+2 : i+end]
		if !validName(name) {
			return "", fmt.Errorf("${%s}: a variable name is letters, digits and _, not starting with a digit", name)
		}
		v, ok := vars[name]
		if !ok {
			return "", &undefinedError{name}
		}
		b.WriteString(v)
		s = s[i+end+1:]
	}
}

// octalInt is a YAML integer written in octal (0644, 0o644).
var octalInt = regexp.MustCompile(`^0o?[0-7]+$`)

// octalAsText makes each plain scalar under n that YAML reads as an
// integer written in octal (0644, 0o644) the string it is: a module reads
// a mode from the digits, which the number YAML makes of them (420) does
// not keep.
func octalAsText(n *yaml.Node) {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Style == 0 && n.ShortTag() == "!!int" && octalInt.MatchString(n.Value) {
			n.Tag = "!!str"
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			octalAsText(n.Content[i])
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			octalAsText(c)
		}
	}
}

// expandNode expands the variables in every scalar under n, mapping keys
// aside. A plain (unquoted, untagged) scalar is then typed as YAML types
// what it now says: port: ${PORT} with PORT=22 is the number 22, as if
// 22 were written there. A quoted one stays a string.
func expandNode(file string, n *yaml.Node, vars map[string]string) error {
	switch n.Kind {
	case yaml.ScalarNode:
		v, err := expand(n.Value, vars)
		if err != nil {
			return errAt(file, n, "%w", err)
		}
		if v != n.Value {
			n.Value = v
			if n.Style == 0 {
				n.Tag = ""
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if err := expandNode(file, n.Content[i], vars); err != nil {
				return err
			}
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			if err := expandNode(file, c, vars); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return errAt(file, n, "aliases are not supported")
	}
	return nil
}
