package yamlite

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	src := `
# leading comment
name: demo   # trailing comment
count: 3
ratio: 0.5
on: true
none: null
empty:
quoted: 'a: b # not a comment'
dq: "tab\there"
version: 3.0.3
list: [a, 'b, c', 2]
nothing: {}
items:
  - one
  - key: v
    other: 2
  -
    deep: x
same_indent:
- p
- q
nested:
  '201':
    $ref: '#/x/y'
  /v1/leases/{id}:
    get: ok
`
	got, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name": "demo", "count": int64(3), "ratio": 0.5, "on": true, "none": nil, "empty": nil,
		"quoted": "a: b # not a comment", "dq": "tab\there", "version": "3.0.3",
		"list":    []any{"a", "b, c", int64(2)},
		"nothing": map[string]any{},
		"items": []any{
			"one",
			map[string]any{"key": "v", "other": int64(2)},
			map[string]any{"deep": "x"},
		},
		"same_indent": []any{"p", "q"},
		"nested": map[string]any{
			"201":             map[string]any{"$ref": "#/x/y"},
			"/v1/leases/{id}": map[string]any{"get": "ok"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"tab indent":      "a:\n\tb: 1",
		"duplicate key":   "a: 1\na: 2",
		"anchor":          "a: &x 1",
		"alias":           "a: *x",
		"tag":             "a: !!str 1",
		"block scalar":    "a: |\n  text",
		"multi document":  "a: 1\n---\nb: 2",
		"flow map":        "a: {b: 1}",
		"nested flow":     "a: [[1]]",
		"unquoted colon":  "a: b: c",
		"bad indent":      "a:\n    b: 1\n  c: 2",
		"scalar then map": "a: 1\n  b: 2",
		"open quote":      "a: 'oops",
		"list in map":     "a: 1\n- b",
	}
	for name, src := range cases {
		if v, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: parsed to %#v, want an error", name, v)
		} else if !strings.Contains(err.Error(), "line ") {
			t.Errorf("%s: error %q lacks a line number", name, err)
		}
	}
}
