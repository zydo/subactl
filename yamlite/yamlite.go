// Package yamlite parses a strict subset of YAML using only the standard
// library. It exists because the project takes no third-party dependencies.
//
// Supported: block maps, block lists, single-line flow lists of scalars
// ([a, b]), the empty flow map ({}), plain and quoted scalars, and comments.
// Anything else (anchors, aliases, tags, block scalars, multiple documents,
// tabs for indentation, duplicate keys) is an error rather than a guess.
//
// Values decode to map[string]any, []any, string, bool, int64, float64 or nil.
package yamlite

import (
	"fmt"
	"strconv"
	"strings"
)

type line struct {
	num    int
	indent int
	text   string // content without indentation or trailing comment
}

type parser struct {
	lines []line
	pos   int
}

// Parse decodes one document.
func Parse(src []byte) (any, error) {
	p := &parser{}
	for i, raw := range strings.Split(string(src), "\n") {
		raw = strings.TrimRight(raw, "\r")
		num := i + 1
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		if indent < len(raw) && raw[indent] == '\t' {
			return nil, fmt.Errorf("line %d: tab in indentation", num)
		}
		text, err := stripComment(raw[indent:])
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", num, err)
		}
		text = strings.TrimRight(text, " ")
		if text == "" {
			continue
		}
		if text == "---" || text == "..." || strings.HasPrefix(text, "--- ") {
			return nil, fmt.Errorf("line %d: multiple documents are not supported", num)
		}
		p.lines = append(p.lines, line{num, indent, text})
	}
	if len(p.lines) == 0 {
		return nil, nil
	}
	v, err := p.block(p.lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		l := p.lines[p.pos]
		return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
	}
	return v, nil
}

// stripComment removes a trailing comment: a '#' at the start of the content
// or after a space, outside quotes.
func stripComment(s string) (string, error) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '"' && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			// A quote only opens a scalar at the start of a token.
			if i == 0 || s[i-1] == ' ' || s[i-1] == '[' || s[i-1] == ',' {
				quote = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i], nil
		}
	}
	if quote != 0 {
		return "", fmt.Errorf("unterminated quoted scalar")
	}
	return s, nil
}

func isSeqItem(t string) bool { return t == "-" || strings.HasPrefix(t, "- ") }

func (p *parser) block(indent int) (any, error) {
	l := p.lines[p.pos]
	if l.indent != indent {
		return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
	}
	if isSeqItem(l.text) {
		return p.seq(indent)
	}
	return p.mapping(indent)
}

func (p *parser) seq(indent int) (any, error) {
	out := []any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent || (l.indent == indent && !isSeqItem(l.text)) {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			p.pos++
			v, err := p.nested(indent, false)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		if _, _, ok, err := splitKey(rest, l.num); err != nil {
			return nil, err
		} else if ok {
			// "- key: value" opens a map whose keys align with this key.
			inner := l.indent + (len(l.text) - len(rest))
			p.lines[p.pos] = line{l.num, inner, rest}
			v, err := p.mapping(inner)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		v, err := scalarOrFlow(rest, l.num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.pos++
	}
	return out, nil
}

func (p *parser) mapping(indent int) (any, error) {
	out := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
		}
		if isSeqItem(l.text) {
			return nil, fmt.Errorf("line %d: list item where a map key was expected", l.num)
		}
		key, rest, ok, err := splitKey(l.text, l.num)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", l.num)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.num, key)
		}
		p.pos++
		var v any
		if rest == "" {
			v, err = p.nested(indent, true)
		} else {
			v, err = scalarOrFlow(rest, l.num)
		}
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}

// nested parses the block that belongs to a key or list item with no inline
// value. A list may sit at the same indentation as its key.
func (p *parser) nested(indent int, sameIndentSeq bool) (any, error) {
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	next := p.lines[p.pos]
	if next.indent > indent {
		return p.block(next.indent)
	}
	if sameIndentSeq && next.indent == indent && isSeqItem(next.text) {
		return p.seq(indent)
	}
	return nil, nil
}

// splitKey splits "key: rest". ok is false when s is not a map entry.
func splitKey(s string, num int) (key, rest string, ok bool, err error) {
	if s[0] == '"' || s[0] == '\'' {
		end, err := closingQuote(s)
		if err != nil {
			return "", "", false, fmt.Errorf("line %d: %v", num, err)
		}
		after := s[end+1:]
		if after != ":" && !strings.HasPrefix(after, ": ") {
			return "", "", false, nil
		}
		key, err = unquote(s[:end+1])
		if err != nil {
			return "", "", false, fmt.Errorf("line %d: %v", num, err)
		}
		return key, strings.TrimLeft(after[1:], " "), true, nil
	}
	if s[0] == '[' || s[0] == '{' {
		return "", "", false, nil
	}
	i := strings.Index(s, ": ")
	if i < 0 {
		if !strings.HasSuffix(s, ":") {
			return "", "", false, nil
		}
		i = len(s) - 1
	}
	key = s[:i]
	if key == "" || strings.ContainsAny(key[:1], "&*!|>") {
		return "", "", false, fmt.Errorf("line %d: unsupported key %q", num, key)
	}
	return key, strings.TrimLeft(s[i+1:], " "), true, nil
}

func closingQuote(s string) (int, error) {
	q := s[0]
	for i := 1; i < len(s); i++ {
		if q == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i, nil
		}
	}
	return 0, fmt.Errorf("unterminated quoted scalar")
}

func unquote(s string) (string, error) {
	if s[0] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	v, err := strconv.Unquote(s)
	if err != nil {
		return "", fmt.Errorf("bad double-quoted scalar %s", s)
	}
	return v, nil
}

func scalarOrFlow(s string, num int) (any, error) {
	switch s[0] {
	case '[':
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("line %d: flow list must close on the same line", num)
		}
		return flowSeq(s[1:len(s)-1], num)
	case '{':
		if strings.ReplaceAll(s, " ", "") == "{}" {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("line %d: flow maps other than {} are not supported", num)
	}
	return scalar(s, num)
}

func flowSeq(body string, num int) (any, error) {
	out := []any{}
	body = strings.TrimSpace(body)
	for body != "" {
		var tok string
		if body[0] == '"' || body[0] == '\'' {
			end, err := closingQuote(body)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", num, err)
			}
			tok, body = body[:end+1], strings.TrimSpace(body[end+1:])
			if body != "" && body[0] != ',' {
				return nil, fmt.Errorf("line %d: expected ',' in flow list", num)
			}
		} else {
			i := strings.IndexByte(body, ',')
			if i < 0 {
				i = len(body)
			}
			tok, body = strings.TrimSpace(body[:i]), body[i:]
			if tok == "" || strings.ContainsAny(tok, "[]{}") {
				return nil, fmt.Errorf("line %d: flow lists may only hold scalars", num)
			}
		}
		v, err := scalar(tok, num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		body = strings.TrimSpace(strings.TrimPrefix(body, ","))
	}
	return out, nil
}

func scalar(s string, num int) (any, error) {
	if s[0] == '"' || s[0] == '\'' {
		end, err := closingQuote(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", num, err)
		}
		if end != len(s)-1 {
			return nil, fmt.Errorf("line %d: text after quoted scalar", num)
		}
		v, err := unquote(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", num, err)
		}
		return v, nil
	}
	switch s[0] {
	case '&', '*', '!', '|', '>', '%', '@', '`':
		return nil, fmt.Errorf("line %d: unsupported YAML feature at %q", num, s)
	}
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") {
		return nil, fmt.Errorf("line %d: plain scalar %q contains ':'; quote it", num, s)
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "~":
		return nil, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	if c := s[0]; c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9') {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, nil
		}
	}
	return s, nil
}
