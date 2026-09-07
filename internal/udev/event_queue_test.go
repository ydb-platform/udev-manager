package udev

import (
	"sync"
	"testing"
	"time"

	"github.com/ydb-platform/udev-manager/internal/mux"
)

func receiveEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("event stream closed early")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("event was not delivered")
		return nil
	}
}

func TestDiscoverySlowSubscriberRetainsEvents(t *testing.T) {
	source := NewFakeDiscovery()
	defer source.Close()
	dev := NewFakeDevice("disk1")
	source.AddDevice(dev)

	fast := make(chan Event, 8)
	stopFast := source.Subscribe(mux.SinkFromChan(fast))
	defer stopFast()
	slow := make(chan Event, 1)
	stopSlow := source.Subscribe(mux.SinkFromChan(slow))
	defer func() {
		// Also release delivery if an assertion fails while the sink is blocked.
		go func() {
			for range slow {
			}
		}()
		stopSlow()
	}()

	// Init fills the slow subscriber. Add blocks its next delivery; the
	// removal and re-add must still reach the other subscriber in order.
	events := []Event{Added{dev}, Removed{dev}, Added{dev}}
	for _, ev := range events {
		source.Emit(ev)
	}
	for _, stream := range []<-chan Event{fast, slow} {
		init, ok := receiveEvent(t, stream).(Init)
		if !ok || len(init.Devices) != 1 || init.Devices[0] != dev {
			t.Fatalf("unexpected Init: %+v", init)
		}
		for _, want := range events {
			if got := receiveEvent(t, stream); got != want {
				t.Fatalf("event = %+v, want %+v", got, want)
			}
		}
	}
}

func TestEventQueueCloseDrainsAcceptedEvents(t *testing.T) {
	events := make(chan Event)
	q := newEventQueue(mux.SinkFromChan(events))
	dev := NewFakeDevice("disk1")
	want := []Event{Init{Devices: []Device{dev}}, Removed{dev}}
	for _, ev := range want {
		if err := q.Submit(ev); err != nil {
			t.Fatal(err)
		}
	}
	var closers sync.WaitGroup
	for i := 0; i < 2; i++ {
		closers.Add(1)
		go func() { defer closers.Done(); q.Close() }()
	}
	for range want {
		receiveEvent(t, events)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected extra event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not close the subscriber")
	}
	closers.Wait()
	if err := q.Submit(Removed{dev}); err == nil {
		t.Fatal("submission after Close succeeded")
	}
}
