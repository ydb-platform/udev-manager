package udev

import (
	"sync"
	"testing"
	"time"
)

func TestNewDiscoveryRejectsNonPositiveReconcileInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		wg := &sync.WaitGroup{}
		discovery, err := NewDiscovery(wg, WithReconcileInterval(interval))
		if err == nil {
			t.Fatalf("NewDiscovery(%s) returned no error", interval)
		}
		if discovery != nil {
			t.Fatalf("NewDiscovery(%s) returned a non-nil discovery", interval)
		}
		wg.Wait()
	}
}

func deviceSet(ids ...Id) map[Id]Device {
	m := make(map[Id]Device, len(ids))
	for _, id := range ids {
		m[id] = NewFakeDevice(id)
	}
	return m
}

func idsOf(devs []Device) map[Id]bool {
	m := make(map[Id]bool, len(devs))
	for _, d := range devs {
		m[d.Id()] = true
	}
	return m
}

func TestApplyEnumeration(t *testing.T) {
	tests := []struct {
		name        string
		state       []Id
		found       []Id
		wantAdded   []Id
		wantRemoved []Id
	}{
		{
			name:      "initial population",
			state:     nil,
			found:     []Id{"a", "b"},
			wantAdded: []Id{"a", "b"},
		},
		{
			name:  "no drift",
			state: []Id{"a", "b"},
			found: []Id{"a", "b"},
		},
		{
			name:      "missed addition",
			state:     []Id{"a"},
			found:     []Id{"a", "b"},
			wantAdded: []Id{"b"},
		},
		{
			name:        "missed removal",
			state:       []Id{"a", "b"},
			found:       []Id{"a"},
			wantRemoved: []Id{"b"},
		},
		{
			name:        "both directions",
			state:       []Id{"a", "b"},
			found:       []Id{"b", "c"},
			wantAdded:   []Id{"c"},
			wantRemoved: []Id{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := deviceSet(tt.state...)
			found := deviceSet(tt.found...)

			added, removed := applyEnumeration(state, found)

			gotAdded := idsOf(added)
			gotRemoved := idsOf(removed)
			if len(gotAdded) != len(tt.wantAdded) {
				t.Fatalf("added = %v, want %v", gotAdded, tt.wantAdded)
			}
			for _, id := range tt.wantAdded {
				if !gotAdded[id] {
					t.Errorf("expected %q in added set %v", id, gotAdded)
				}
			}
			if len(gotRemoved) != len(tt.wantRemoved) {
				t.Fatalf("removed = %v, want %v", gotRemoved, tt.wantRemoved)
			}
			for _, id := range tt.wantRemoved {
				if !gotRemoved[id] {
					t.Errorf("expected %q in removed set %v", id, gotRemoved)
				}
			}

			if len(state) != len(found) {
				t.Fatalf("state has %d devices after reconcile, want %d", len(state), len(found))
			}
			for id := range found {
				if _, ok := state[id]; !ok {
					t.Errorf("expected %q in state after reconcile", id)
				}
			}

			// Devices that were already tracked must keep their identity so
			// downstream consumers holding references stay valid.
			for _, id := range tt.state {
				if contains(tt.found, id) && state[id] == found[id] {
					t.Errorf("device %q was replaced by the enumeration copy; existing entry must be kept", id)
				}
			}
		})
	}
}

func contains(ids []Id, id Id) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
