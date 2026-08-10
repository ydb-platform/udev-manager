package udev

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydb-platform/udev-manager/internal/mux"
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
	result := make(map[Id]Device, len(ids))
	for _, id := range ids {
		result[id] = NewFakeDevice(id)
	}
	return result
}

func TestEnumerationDelta(t *testing.T) {
	tests := []struct {
		name        string
		current     []Id
		next        []Id
		wantAdded   int
		wantRemoved int
	}{
		{name: "empty", current: nil, next: nil},
		{name: "initial population", next: []Id{"a", "b"}, wantAdded: 2},
		{name: "no key drift", current: []Id{"a", "b"}, next: []Id{"a", "b"}},
		{name: "both directions", current: []Id{"a", "b"}, next: []Id{"b", "c"}, wantAdded: 1, wantRemoved: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			added, removed := enumerationDelta(deviceSet(test.current...), deviceSet(test.next...))
			if added != test.wantAdded || removed != test.wantRemoved {
				t.Fatalf("enumerationDelta() = (%d, %d), want (%d, %d)", added, removed, test.wantAdded, test.wantRemoved)
			}
		})
	}
}

func TestDeviceSnapshotIsSorted(t *testing.T) {
	snapshot := deviceSnapshot(deviceSet("c", "a", "b"))
	for i, want := range []Id{"a", "b", "c"} {
		if got := snapshot[i].Id(); got != want {
			t.Fatalf("snapshot[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestSignalReconcileCoalesces(t *testing.T) {
	discovery := &udevDiscovery{reconcileC: make(chan struct{}, 1)}
	for i := 0; i < 10_000; i++ {
		discovery.signalReconcile()
	}
	if got := len(discovery.reconcileC); got != 1 {
		t.Fatalf("pending reconcile signals = %d, want 1", got)
	}
}

func newControllerTestDiscovery(requestBuffer int) *udevDiscovery {
	ctx, cancel := context.WithCancel(context.Background())
	monitorReady := make(chan struct{})
	close(monitorReady)
	return &udevDiscovery{
		ctx:               ctx,
		cancel:            cancel,
		state:             make(map[Id]Device),
		requests:          make(chan mux.AwaitReply[monitorRequest, any], requestBuffer),
		mux:               mux.Make[Snapshot](),
		done:              make(chan struct{}),
		monitorReady:      monitorReady,
		reconcileC:        make(chan struct{}, 1),
		reconcileInterval: time.Hour,
	}
}

func startControllerTestMonitor(t *testing.T, discovery *udevDiscovery, reconcile func() bool, retryInterval time.Duration) {
	t.Helper()
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go discovery.runMonitor(wg, reconcile, retryInterval)
	t.Cleanup(func() {
		discovery.cancel()
		select {
		case <-discovery.done:
		case <-time.After(time.Second):
			t.Fatal("controller did not stop after cancellation")
		}
		wg.Wait()
	})
}

func setControllerTestState(discovery *udevDiscovery, generation uint64, ids ...Id) {
	discovery.mu.Lock()
	discovery.state = deviceSet(ids...)
	discovery.generation = generation
	discovery.mu.Unlock()
}

func waitForControllerSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestFailedReconcileRetainsStateAndDoesNotBlockRequests(t *testing.T) {
	discovery := newControllerTestDiscovery(0)
	initialDone := make(chan struct{})
	failed := make(chan struct{})
	var calls atomic.Int32
	reconcile := func() bool {
		switch calls.Add(1) {
		case 1:
			setControllerTestState(discovery, 1, "last-good")
			close(initialDone)
			return true
		case 2:
			close(failed)
			return false
		default:
			return true
		}
	}

	// A long retry interval makes it explicit that request handling does not
	// wait for the retry timer.
	startControllerTestMonitor(t, discovery, reconcile, time.Hour)
	waitForControllerSignal(t, initialDone, "initial reconciliation")
	discovery.signalReconcile()
	waitForControllerSignal(t, failed, "failed reconciliation")

	stateResult := make(chan map[Id]Device, 1)
	go func() {
		stateResult <- discovery.State(mux.Any[Device]())
	}()
	select {
	case state := <-stateResult:
		if _, ok := state["last-good"]; !ok || len(state) != 1 {
			t.Fatalf("State() after failed reconcile = %v, want last-good state", state)
		}
	case <-time.After(time.Second):
		t.Fatal("State() blocked behind the retry timer")
	}

	snapshots := make(chan Snapshot, 1)
	cancelResult := make(chan mux.CancelFunc, 1)
	go func() {
		cancelResult <- discovery.Subscribe(mux.SinkFromChan(snapshots))
	}()

	var cancel mux.CancelFunc
	select {
	case cancel = <-cancelResult:
	case <-time.After(time.Second):
		t.Fatal("Subscribe() blocked behind the retry timer")
	}
	defer cancel()

	select {
	case snapshot := <-snapshots:
		if snapshot.Generation != 1 || len(snapshot.Devices) != 1 || snapshot.Devices[0].Id() != "last-good" {
			t.Fatalf("initial snapshot after failed reconcile = %+v, want generation 1 with last-good", snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe() did not replay the last-good snapshot")
	}
}

func TestFailedReconcileSchedulesRetry(t *testing.T) {
	discovery := newControllerTestDiscovery(0)
	initialDone := make(chan struct{})
	failed := make(chan struct{})
	retried := make(chan struct{})
	var calls atomic.Int32
	reconcile := func() bool {
		switch calls.Add(1) {
		case 1:
			setControllerTestState(discovery, 1, "old")
			close(initialDone)
			return true
		case 2:
			close(failed)
			return false
		case 3:
			setControllerTestState(discovery, 2, "new")
			close(retried)
			return true
		default:
			return true
		}
	}

	startControllerTestMonitor(t, discovery, reconcile, 10*time.Millisecond)
	waitForControllerSignal(t, initialDone, "initial reconciliation")
	discovery.signalReconcile()
	waitForControllerSignal(t, failed, "failed reconciliation")
	waitForControllerSignal(t, retried, "scheduled retry")

	state := discovery.State(mux.Any[Device]())
	if _, ok := state["new"]; !ok || len(state) != 1 {
		t.Fatalf("State() after successful retry = %v, want new state", state)
	}
}

func TestFirstSubscriberWaitsForPendingInitialScanReconcile(t *testing.T) {
	// Buffer the request so it and the dirty notification are both pending
	// when the initial scan returns. The controller must reconcile first no
	// matter which ready case its outer select chooses.
	discovery := newControllerTestDiscovery(1)
	initialStarted := make(chan struct{})
	releaseInitial := make(chan struct{})
	trailingDone := make(chan struct{})
	var calls atomic.Int32
	reconcile := func() bool {
		switch calls.Add(1) {
		case 1:
			setControllerTestState(discovery, 1, "before-event")
			discovery.signalReconcile()
			close(initialStarted)
			<-releaseInitial
			return true
		case 2:
			setControllerTestState(discovery, 2, "after-event")
			close(trailingDone)
			return true
		default:
			return true
		}
	}

	startControllerTestMonitor(t, discovery, reconcile, time.Hour)
	waitForControllerSignal(t, initialStarted, "initial reconciliation to start")

	snapshots := make(chan Snapshot, 1)
	request := mux.NewAwaitReply[monitorRequest, any](newSub{sink: mux.SinkFromChan(snapshots)})
	discovery.requests <- request
	close(releaseInitial)

	replyResult := make(chan any, 1)
	go func() {
		replyResult <- request.Await()
	}()

	waitForControllerSignal(t, trailingDone, "event-triggered trailing reconciliation")
	var cancel mux.CancelFunc
	select {
	case reply := <-replyResult:
		cancel = reply.(mux.CancelFunc)
	case <-time.After(time.Second):
		t.Fatal("first subscription was not accepted after trailing reconciliation")
	}
	defer cancel()

	select {
	case snapshot := <-snapshots:
		if snapshot.Generation != 2 || len(snapshot.Devices) != 1 || snapshot.Devices[0].Id() != "after-event" {
			t.Fatalf("first snapshot = %+v, want generation 2 after pending reconciliation", snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("first subscriber did not receive an initial snapshot")
	}
}
