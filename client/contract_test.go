package client

import (
	"testing"

	"github.com/zydo/subactl/internal/contract"
)

// pending lists contract operations the client does not implement yet, with
// the milestone that adds each. An entry must be removed when its method
// lands, and the test fails if it is not.
var pending = map[string]string{
	"attach":         "M4",
	"requestResize":  "M4",
	"notify":         "M4",
	"reportProgress": "M4",
	"streamInbox":    "M4",
	"getTrace":       "M3",
	"streamEvents":   "M3",
	"getMetrics":     "M3",
}

func TestClientMatchesContract(t *testing.T) {
	c, err := contract.Load()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]operation{}
	for _, op := range operations {
		have[op.id] = op
	}
	for _, want := range c.Operations {
		got, ok := have[want.ID]
		_, isPending := pending[want.ID]
		switch {
		case ok && isPending:
			t.Errorf("%s is implemented but still listed as pending", want.ID)
		case !ok && !isPending:
			t.Errorf("contract operation %s %s %s has no client method", want.ID, want.Method, want.Path)
		case ok && (got.method != want.Method || got.path != want.Path):
			t.Errorf("%s: client uses %s %s, contract says %s %s", want.ID, got.method, got.path, want.Method, want.Path)
		}
		delete(have, want.ID)
		delete(pending, want.ID)
	}
	for id := range have {
		t.Errorf("client operation %s is not in the contract", id)
	}
	for id := range pending {
		t.Errorf("pending operation %s is not in the contract", id)
	}
}
