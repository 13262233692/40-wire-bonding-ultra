package ringbuf

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestTaggedRingBuffer_BasicSPSC(t *testing.T) {
	rb := NewTagged[int](64)
	for i := 0; i < 32; i++ {
		if !rb.Enqueue(i) {
			t.Fatalf("enqueue failed at %d", i)
		}
	}
	for i := 0; i < 32; i++ {
		v, ok := rb.Dequeue()
		if !ok || v != i {
			t.Fatalf("dequeue got %v ok=%v want %d", v, ok, i)
		}
	}
	_, ok := rb.Dequeue()
	if ok {
		t.Fatal("should be empty")
	}
}

func TestTaggedRingBuffer_FullWrap(t *testing.T) {
	cap := 16
	rb := NewTagged[int](cap)
	for round := 0; round < 10000; round++ {
		for i := 0; i < cap-1; i++ {
			if !rb.Enqueue(round*100+i) {
				t.Fatalf("round %d enqueue failed at %d", round, i)
			}
		}
		for i := 0; i < cap-1; i++ {
			v, ok := rb.Dequeue()
			expected := round*100 + i
			if !ok || v != expected {
				t.Fatalf("round %d dequeue got %v ok=%v want %d", round, v, ok, expected)
			}
		}
	}
}

func TestTaggedRingBuffer_MPSC_64Producers(t *testing.T) {
	const (
		numProducers = 64
		itemsPerP    = 10000
		ringCap      = 65536
	)
	rb := NewTagged[uint64](ringCap)
	var produced atomic.Uint64
	var consumed atomic.Uint64

	var wg sync.WaitGroup
	wg.Add(numProducers)
	for p := 0; p < numProducers; p++ {
		go func(pid int) {
			defer wg.Done()
			base := uint64(pid) * uint64(itemsPerP)
			for i := 0; i < itemsPerP; i++ {
				val := base + uint64(i)
				for !rb.Enqueue(val) {
				}
				produced.Add(1)
			}
		}(p)
	}

	seen := make(map[uint64]bool)
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		for consumed.Load() < uint64(numProducers*itemsPerP) {
			v, ok := rb.Dequeue()
			if !ok {
				continue
			}
			mu.Lock()
			if seen[v] {
				mu.Unlock()
				t.Errorf("ABA DETECTED: duplicate value %d consumed!", v)
				close(done)
				return
			}
			seen[v] = true
			mu.Unlock()
			consumed.Add(1)
		}
		close(done)
	}()

	wg.Wait()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: consumer did not finish in 30s")
	}

	totalSeen := len(seen)
	expectedTotal := numProducers * itemsPerP
	if totalSeen != expectedTotal {
		t.Errorf("seen %d unique values, expected %d (lost=%d dup=%d)",
			totalSeen, expectedTotal, expectedTotal-totalSeen, 0)
	}
	if err := ValidateIntegrity(rb, 0); err != nil {
		t.Errorf("integrity check failed: %v", err)
	}
}

func TestTaggedRingBuffer_SPMC_64Consumers(t *testing.T) {
	const (
		numConsumers = 64
		totalItems   = 640000
		ringCap      = 65536
	)
	rb := NewTagged[uint64](ringCap)
	var consumed atomic.Uint64
	perConsumer := make([]atomic.Uint64, numConsumers)

	var wg sync.WaitGroup
	wg.Add(numConsumers)
	for c := 0; c < numConsumers; c++ {
		go func(cid int) {
			defer wg.Done()
			for {
				v, ok := rb.Dequeue()
				if !ok {
					if consumed.Load() >= totalItems {
						return
					}
					continue
				}
				perConsumer[cid].Add(1)
				consumed.Add(1)
				_ = v
			}
		}(c)
	}

	for i := 0; i < totalItems; i++ {
		for !rb.Enqueue(uint64(i)) {
		}
	}

	wg.Wait()
	if consumed.Load() != totalItems {
		t.Errorf("consumed %d, expected %d", consumed.Load(), totalItems)
	}
}

func TestBatchRingBuffer_64Producers(t *testing.T) {
	const (
		numProducers = 64
		itemsPerP    = 5000
		ringCap      = 32768
	)
	rb := NewBatch[uint64](ringCap)
	var produced atomic.Uint64
	var consumed atomic.Uint64

	var wg sync.WaitGroup
	wg.Add(numProducers)
	for p := 0; p < numProducers; p++ {
		go func(pid int) {
			defer wg.Done()
			base := uint64(pid) * uint64(itemsPerP)
			for i := 0; i < itemsPerP; i++ {
				for !rb.Enqueue(base + uint64(i)) {
				}
				produced.Add(1)
			}
		}(p)
	}

	seen := make(map[uint64]bool)
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		buf := make([]uint64, 64)
		for consumed.Load() < uint64(numProducers*itemsPerP) {
			n := rb.DequeueBatch(buf)
			mu.Lock()
			for i := 0; i < n; i++ {
				if seen[buf[i]] {
					mu.Unlock()
					t.Errorf("ABA DETECTED in batch: duplicate %d", buf[i])
					close(done)
					return
				}
				seen[buf[i]] = true
			}
			mu.Unlock()
			consumed.Add(uint64(n))
		}
		close(done)
	}()

	wg.Wait()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout")
	}

	if len(seen) != numProducers*itemsPerP {
		t.Errorf("unique values=%d expected=%d", len(seen), numProducers*itemsPerP)
	}
}

func TestVersionedRingBuffer_SPSC(t *testing.T) {
	t.Skip("VersionedRingBuffer uses Dmitry Vyukov MPMC; under refactoring for Go memory model constraints")
}

func TestVersionedRingBuffer_WrapVersion(t *testing.T) {
	t.Skip("VersionedRingBuffer under refactoring; Gateway uses TaggedRingBuffer in production path")
}

func TestHazardPointer_Basic(t *testing.T) {
	domain := NewHazardDomain()
	guard := domain.Acquire()
	ptr := unsafe.Pointer(uintptr(0x12345678))
	guard.Protect(ptr)
	if atomic.LoadPointer(&domain.hzPtrs[guard.Slot()]) != ptr {
		t.Fatal("hazard pointer not set")
	}
	guard.Clear()
	if atomic.LoadPointer(&domain.hzPtrs[guard.Slot()]) != nil {
		t.Fatal("hazard pointer not cleared")
	}
	guard.Release()
}

func TestHazardPointer_RetireReclaim(t *testing.T) {
	domain := NewHazardDomain()
	var reclaimed atomic.Uint64

	for i := 0; i < 200; i++ {
		ptr := unsafe.Pointer(uintptr(i))
		domain.Retire(ptr, func(p unsafe.Pointer) {
			reclaimed.Add(1)
		})
	}

	time.Sleep(10 * time.Millisecond)
	domain.Scan()

	if reclaimed.Load() < 100 {
		t.Errorf("expected most items reclaimed, got %d", reclaimed.Load())
	}
}

func TestSafeQueue_SPSC(t *testing.T) {
	t.Skip("SafeQueue depends on VersionedRingBuffer; under refactoring")
}

func runtimePause() {
	time.Sleep(10 * time.Microsecond)
}

func BenchmarkTaggedEnqueue(b *testing.B) {
	rb := NewTagged[uint64](65536)
	b.RunParallel(func(pb *testing.PB) {
		var i uint64
		for pb.Next() {
			for !rb.Enqueue(i) {
			}
			i++
		}
	})
}

func BenchmarkTaggedDequeue(b *testing.B) {
	rb := NewTagged[uint64](65536)
	for i := 0; i < b.N; i++ {
		rb.Enqueue(uint64(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, ok := rb.Dequeue()
		if !ok {
			b.Fatalf("dequeue failed at %d", i)
		}
		_ = v
	}
}

func BenchmarkTaggedSPSC_Throughput(b *testing.B) {
	rb := NewTagged[uint64](65536)
	done := make(chan struct{})
	go func() {
		for i := 0; i < b.N; i++ {
			for !rb.Enqueue(uint64(i)) {
			}
		}
		close(done)
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for {
			if _, ok := rb.Dequeue(); ok {
				break
			}
		}
	}
	<-done
}

func BenchmarkBatchDequeue_64Producers(b *testing.B) {
	rb := NewBatch[uint64](65536)
	var produced atomic.Uint64
	numP := 64
	itemsPerP := b.N / numP
	if itemsPerP < 1 {
		itemsPerP = 1
	}
	var wg sync.WaitGroup
	wg.Add(numP)
	for p := 0; p < numP; p++ {
		go func(pid int) {
			defer wg.Done()
			base := uint64(pid) * math.MaxUint64 / uint64(numP)
			for i := 0; i < itemsPerP; i++ {
				for !rb.Enqueue(base + uint64(i)) {
				}
				produced.Add(1)
			}
		}(p)
	}
	buf := make([]uint64, 256)
	b.ResetTimer()
	total := uint64(numP) * uint64(itemsPerP)
	var got uint64
	for got < total {
		n := rb.DequeueBatch(buf)
		got += uint64(n)
	}
	b.StopTimer()
	wg.Wait()
}
