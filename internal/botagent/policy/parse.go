// Vendored from bot-agent internal/agent/policy (Apache-2.0, see ../NOTICE).
// DD changes: the key set is a caller-supplied Schema instead of bot-agent's
// fixed keys, and a scalar basic string is accepted. The value parsers
// (parseBool, parseInt, parseArray, parseString) are unchanged.
package policy

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is a key's TOML type in the subset: bool, non-negative int, basic
// string, or array of basic strings.
type Kind int

const (
	Bool Kind = iota
	Int
	String
	Strings
)

// Value is one parsed key. Only the field for its Kind is set.
type Value struct {
	Bool    bool
	Int     int
	String  string
	Strings []string
}

// Schema is the set of keys a file may hold. Anything else is unknown_key.
type Schema map[string]Kind

// parseTOML accepts the schema's keys, one "key = value" per line. Unknown
// keys, duplicates, and wrong types return a fixed reason. Values are not
// included in the reason.
func parseTOML(data []byte, schema Schema) (map[string]Value, string) {
	if !utf8.Valid(data) || strings.Contains(string(data), "\x00") {
		return nil, "parse"
	}
	out := map[string]Value{}
	for _, line := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		key, rest, ok := strings.Cut(trim, "=")
		if !ok {
			return nil, "parse"
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimSpace(rest)
		if key == "" || strings.ContainsAny(key, " \t#") {
			return nil, "parse"
		}
		kind, known := schema[key]
		if !known {
			return nil, "unknown_key"
		}
		if _, dup := out[key]; dup {
			return nil, "parse"
		}
		var v Value
		switch kind {
		case Bool:
			b, ok := parseBool(rest)
			if !ok {
				return nil, "parse"
			}
			v.Bool = b
		case Int:
			n, ok := parseInt(rest)
			if !ok {
				return nil, "parse"
			}
			v.Int = n
		case String:
			str, tail, ok := parseString(rest)
			if !ok || strings.TrimSpace(tail) != "" {
				return nil, "parse"
			}
			v.String = str
		case Strings:
			xs, ok := parseArray(rest)
			if !ok {
				return nil, "parse"
			}
			v.Strings = xs
		default:
			return nil, "parse"
		}
		out[key] = v
	}
	return out, ""
}

func parseBool(s string) (bool, bool) {
	switch s {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func parseInt(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func parseArray(s string) ([]string, bool) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, false
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		return []string{}, true
	}
	var out []string
	for len(body) > 0 {
		body = strings.TrimSpace(body)
		if body == "" {
			break
		}
		if body[0] != '"' {
			return nil, false
		}
		val, rest, ok := parseString(body)
		if !ok {
			return nil, false
		}
		out = append(out, val)
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return nil, false
		}
		body = strings.TrimSpace(rest[1:])
		if body == "" {
			return nil, false
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, true
}

// parseString reads a double-quoted TOML basic string. Only \\ and \" are escapes.
func parseString(s string) (string, string, bool) {
	if s == "" || s[0] != '"' {
		return "", "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return b.String(), s[i+1:], true
		}
		if c == '\n' || c == '\r' || c == 0 {
			return "", "", false
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			return "", "", false
		}
		n := s[i+1]
		switch n {
		case '\\', '"':
			b.WriteByte(n)
			i++
		default:
			return "", "", false
		}
	}
	return "", "", false
}
