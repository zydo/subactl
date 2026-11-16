package daemon

import (
	"testing"

	"github.com/zydo/subactl/internal/contract"
)

// TestRoutesMatchContract holds the route table against openapi.yaml in
// both directions.
func TestRoutesMatchContract(t *testing.T) {
	c, err := contract.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Operations) != 18 {
		// 17 rows in the design's API table; one row holds two operations.
		t.Errorf("contract has %d operations, want 18", len(c.Operations))
	}
	have := map[string]Route{}
	for _, r := range Routes() {
		if _, dup := have[r.ID]; dup {
			t.Errorf("duplicate route %s", r.ID)
		}
		if (r.handler == nil) == (r.Pending == "") {
			t.Errorf("%s: a route has either a handler or a pending milestone", r)
		}
		have[r.ID] = r
	}
	for _, want := range c.Operations {
		got, ok := have[want.ID]
		if !ok {
			t.Errorf("contract operation %s %s %s has no route", want.ID, want.Method, want.Path)
		} else if got.Method != want.Method || got.Path != want.Path {
			t.Errorf("%s: route is %s %s, contract says %s %s", want.ID, got.Method, got.Path, want.Method, want.Path)
		}
		delete(have, want.ID)
	}
	for _, r := range have {
		t.Errorf("route %s is not in the contract", r)
	}
}
