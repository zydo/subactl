// Package contract reads api/openapi.yaml into the few facts the
// conformance tests need: the operations and the schema property names.
package contract

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zydo/subactl/api"
	"github.com/zydo/subactl/internal/yamlite"
)

// Operation is one method on one path.
type Operation struct {
	ID     string
	Method string // upper case
	Path   string
}

// Contract is the parsed contract.
type Contract struct {
	Operations []Operation
	// Schemas maps a schema name to its property names. Schemas without
	// properties (enums) map to nil.
	Schemas map[string][]string
}

var methods = []string{"get", "post", "put", "patch", "delete"}

// Load parses the embedded contract and checks that every $ref resolves.
func Load() (*Contract, error) {
	doc, err := yamlite.Parse(api.OpenAPI)
	if err != nil {
		return nil, fmt.Errorf("openapi.yaml: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openapi.yaml: top level is not a map")
	}
	c := &Contract{Schemas: map[string][]string{}}
	paths, _ := root["paths"].(map[string]any)
	seen := map[string]bool{}
	for path, item := range paths {
		m, _ := item.(map[string]any)
		for _, method := range methods {
			op, ok := m[method].(map[string]any)
			if !ok {
				continue
			}
			id, _ := op["operationId"].(string)
			if id == "" {
				return nil, fmt.Errorf("%s %s: missing operationId", method, path)
			}
			if seen[id] {
				return nil, fmt.Errorf("duplicate operationId %q", id)
			}
			seen[id] = true
			c.Operations = append(c.Operations, Operation{ID: id, Method: strings.ToUpper(method), Path: path})
		}
	}
	sort.Slice(c.Operations, func(i, j int) bool { return c.Operations[i].ID < c.Operations[j].ID })

	comps, _ := root["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	for name, s := range schemas {
		m, _ := s.(map[string]any)
		props, _ := m["properties"].(map[string]any)
		var names []string
		for p := range props {
			names = append(names, p)
		}
		sort.Strings(names)
		c.Schemas[name] = names
	}
	if err := checkRefs(root, root); err != nil {
		return nil, err
	}
	return c, nil
}

func checkRefs(root map[string]any, v any) error {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if !strings.HasPrefix(ref, "#/") {
				return fmt.Errorf("$ref %q: only local references are supported", ref)
			}
			var cur any = root
			for _, part := range strings.Split(ref[2:], "/") {
				m, ok := cur.(map[string]any)
				if !ok || m[part] == nil {
					return fmt.Errorf("$ref %q does not resolve", ref)
				}
				cur = m[part]
			}
		}
		for _, c := range t {
			if err := checkRefs(root, c); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range t {
			if err := checkRefs(root, c); err != nil {
				return err
			}
		}
	}
	return nil
}
