// Package capacity turns host memory readings into the effective capacity
// of the ledger, and holds the only rule by which subactld acts on its own:
// the safety net.
package capacity

import (
	"sort"

	"github.com/zydo/subactl/api"
)

// Host is one reading of the machine, or of the outer cgroup that stands
// in for it. Sizes are bytes.
type Host struct {
	Total     int64
	Available int64
}

// Reader reads the host.
type Reader interface {
	Read() (Host, error)
}

// External is the memory used by processes subactld does not manage.
// managed is the working set of all top-level acgroups.
//
// It cannot be read off MemAvailable alone: that already has the agents'
// own usage taken out, so the more they used the smaller the capacity
// would look, and the ledger would chase its own tail.
func External(h Host, managed int64) int64 {
	return max(h.Total-h.Available-managed, 0)
}

// Effective is the memory that top-level leases may be given in total.
func Effective(limit int64, h Host, reserve, external int64) int64 {
	eff := max(h.Total-reserve-external, 0)
	if limit > 0 {
		eff = min(eff, limit)
	}
	return eff
}

// Net is the hysteresis of the safety net: memory must be below the
// reserve line for Need consecutive readings before anything happens.
type Net struct {
	Need  int
	below int
}

// Observe records a reading and reports whether the net has tripped.
func (n *Net) Observe(available, reserve int64) bool {
	if available >= reserve {
		n.below = 0
		return false
	}
	n.below++
	return n.below >= n.Need
}

// Node is a live lease as the safety net sees it.
type Node struct {
	ID        api.LeaseID
	Parent    api.LeaseID // empty for a lead
	Lead      api.LeaseID
	Usage     int64 // working set, nested leases included
	KillOrder *int
	Leaf      bool // has no live nested lease
}

// Victim picks the one lease to terminate next. It is a fixed rule, not a
// judgement:
//
//  1. take the lead tree that uses the most memory;
//  2. inside it, if any lease carries a kill_order hint, take the lowest;
//  3. otherwise take a leaf, the one using the most memory.
//
// A hint only ever orders leases inside the tree whose planner gave it.
func Victim(nodes []Node) (api.LeaseID, bool) {
	var lead api.LeaseID
	var most int64 = -1
	for _, n := range nodes {
		if n.Parent == "" && (n.Usage > most || (n.Usage == most && n.ID < lead)) {
			lead, most = n.ID, n.Usage
		}
	}
	if most < 0 {
		return "", false
	}
	var tree []Node
	for _, n := range nodes {
		if n.Lead == lead {
			tree = append(tree, n)
		}
	}
	// Largest usage first, then by id so the choice is reproducible.
	sort.Slice(tree, func(i, j int) bool {
		if tree[i].Usage != tree[j].Usage {
			return tree[i].Usage > tree[j].Usage
		}
		return tree[i].ID < tree[j].ID
	})
	var hinted *Node
	for i := range tree {
		if n := &tree[i]; n.KillOrder != nil && (hinted == nil || *n.KillOrder < *hinted.KillOrder) {
			hinted = n
		}
	}
	if hinted != nil {
		return hinted.ID, true
	}
	for _, n := range tree {
		if n.Leaf {
			return n.ID, true
		}
	}
	return lead, true
}
