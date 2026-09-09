package udev

import (
	"fmt"
	"net/http"
	"sync"
)

var eventQueues sync.Map // Active *eventQueue instances; removed after draining.

// QueueMetrics serves Prometheus gauges for discovery delivery backlog.
// Counts include in-flight delivery, but exclude events already accepted by
// subscriber channels. Init counts as one event regardless of device count.
func QueueMetrics(w http.ResponseWriter, _ *http.Request) {
	var total, largest int64
	eventQueues.Range(func(key, _ any) bool {
		n := key.(*eventQueue).backlog.Load()
		total += n
		largest = max(largest, n)
		return true
	})
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP udev_manager_discovery_queue_events Events awaiting discovery delivery across all queues, including in-flight delivery.\n"+
		"# TYPE udev_manager_discovery_queue_events gauge\n"+
		"udev_manager_discovery_queue_events %d\n"+
		"# HELP udev_manager_discovery_queue_max_events Largest discovery delivery backlog, including in-flight delivery.\n"+
		"# TYPE udev_manager_discovery_queue_max_events gauge\n"+
		"udev_manager_discovery_queue_max_events %d\n", total, largest)
}
