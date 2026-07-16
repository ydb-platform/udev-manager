package mux

import "testing"

func TestRingQueueWraparoundAndGrowth(t *testing.T) {
	var queue ringQueue[int]

	for i := 0; i < initialElasticQueueCapacity; i++ {
		queue.push(i)
	}
	for i := 0; i < initialElasticQueueCapacity/2; i++ {
		value, ok := queue.pop()
		if !ok || value != i {
			t.Fatalf("Pop() = %d, %t; want %d, true", value, ok, i)
		}
	}
	for i := initialElasticQueueCapacity; i < initialElasticQueueCapacity*2; i++ {
		queue.push(i)
	}

	// The final pushes wrap around and then force a growth while head is not
	// zero. Every value must retain FIFO order across both transitions.
	for want := initialElasticQueueCapacity / 2; want < initialElasticQueueCapacity*2; want++ {
		value, ok := queue.pop()
		if !ok || value != want {
			t.Fatalf("Pop() = %d, %t; want %d, true", value, ok, want)
		}
	}
	if queue.len() != 0 {
		t.Fatalf("len() = %d; want 0", queue.len())
	}
}

func TestRingQueueReleasesPathologicalPeak(t *testing.T) {
	var queue ringQueue[*int]
	for i := 0; i <= maxRetainedElasticQueueCapacity; i++ {
		value := i
		queue.push(&value)
	}
	for queue.len() > 0 {
		queue.pop()
	}

	if queue.values != nil {
		t.Fatalf("drained queue retained capacity %d", len(queue.values))
	}
}

func BenchmarkRingQueueSteadyState(b *testing.B) {
	var queue ringQueue[int]
	for i := 0; i < maxRetainedElasticQueueCapacity; i++ {
		queue.push(i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		queue.pop()
		queue.push(i)
	}
}
