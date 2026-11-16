package api_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/contract"
)

// TestSchemasMatchContract checks, in both directions, that every object
// schema in openapi.yaml has a Go type with exactly the same JSON fields.
func TestSchemasMatchContract(t *testing.T) {
	c, err := contract.Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, props := range c.Schemas {
		if props == nil {
			continue // enum or scalar schema
		}
		typ, ok := api.Schemas[name]
		if !ok {
			t.Errorf("schema %s has no Go type in api.Schemas", name)
			continue
		}
		if got := jsonFields(typ); !reflect.DeepEqual(got, props) {
			t.Errorf("schema %s:\n  go   %v\n  yaml %v", name, got, props)
		}
	}
	for name := range api.Schemas {
		if c.Schemas[name] == nil {
			t.Errorf("Go type %s has no object schema in openapi.yaml", name)
		}
	}
}

func jsonFields(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func TestCodesMatchContract(t *testing.T) {
	// Every code has a name, and the names are unique.
	seen := map[string]bool{}
	for _, c := range api.Codes() {
		n := api.CodeName(c)
		if n == "" || seen[n] {
			t.Errorf("code %d: bad or duplicate name %q", c, n)
		}
		seen[n] = true
	}
	if len(seen) != 13 {
		t.Errorf("got %d termination codes, the design defines 13", len(seen))
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"1024": 1024, "1K": 1 << 10, "512M": 512 << 20, "24G": 24 << 30,
		"3GiB": 3 << 30, "1.5g": 3 << 29, "2T": 2 << 40, "64MB": 64 << 20,
	} {
		if got, err := api.ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "G", "-1G", "12X", "1e30G"} {
		if _, err := api.ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) succeeded", bad)
		}
	}
	if got := api.FormatBytes(3 << 30); got != "3GiB" {
		t.Errorf("FormatBytes = %q", got)
	}
}
