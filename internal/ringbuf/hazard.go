package ringbuf

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

const (
	hazardPtrSlots   = 128
	retireBatchSize  = 64
	reclaimThreshold = 3
)

type HazardGuard struct {
	slot  int
	hp    *HazardDomain
	prev  unsafe.Pointer
}

type HazardDomain struct {
	hzPtrs    [hazardPtrSlots]unsafe.Pointer
	inUse     [hazardPtrSlots]atomic.Bool
	globalSeq atomic.Uint64
	retired   struct {
		mu    sync.Mutex
		list  []retiredNode
		count int
	}
}

type retiredNode struct {
	ptr  unsafe.Pointer
 reclaim func(unsafe.Pointer)
	seq   uint64
}

var globalDomain = NewHazardDomain()

func GlobalDomain() *HazardDomain {
	return globalDomain
}

func NewHazardDomain() *HazardDomain {
	return &HazardDomain{}
}

func (hd *HazardDomain) Acquire() *HazardGuard {
	for i := 0; i < hazardPtrSlots; i++ {
		if hd.inUse[i].CompareAndSwap(false, true) {
			return &HazardGuard{slot: i, hp: hd}
		}
	}
	panic("ringbuf: all hazard pointer slots exhausted (increase hazardPtrSlots)")
}

func (hg *HazardGuard) Protect(ptr unsafe.Pointer) {
	hg.prev = atomic.LoadPointer(&hg.hp.hzPtrs[hg.slot])
	atomic.StorePointer(&hg.hp.hzPtrs[hg.slot], ptr)
}

func (hg *HazardGuard) Clear() {
	atomic.StorePointer(&hg.hp.hzPtrs[hg.slot], hg.prev)
	hg.prev = nil
}

func (hg *HazardGuard) Release() {
	atomic.StorePointer(&hg.hp.hzPtrs[hg.slot], nil)
	hg.hp.inUse[hg.slot].Store(false)
}

func (hg *HazardGuard) Slot() int {
	return hg.slot
}

func (hd *HazardDomain) Retire(ptr unsafe.Pointer, reclaim func(unsafe.Pointer)) {
	seq := hd.globalSeq.Add(1)
	hd.retired.mu.Lock()
	hd.retired.list = append(hd.retired.list, retiredNode{ptr: ptr, reclaim: reclaim, seq: seq})
	hd.retired.count++
	shouldScan := hd.retired.count >= retireBatchSize
	hd.retired.mu.Unlock()

	if shouldScan {
		hd.Scan()
	}
}

func (hd *HazardDomain) Scan() {
	hd.retired.mu.Lock()
	defer hd.retired.mu.Unlock()

	hazardSet := make(map[unsafe.Pointer]struct{})
	for i := 0; i < hazardPtrSlots; i++ {
		if hd.inUse[i].Load() {
			ptr := atomic.LoadPointer(&hd.hzPtrs[i])
			if ptr != nil {
				hazardSet[ptr] = struct{}{}
			}
		}
	}

	var remaining []retiredNode
	for _, node := range hd.retired.list {
		if _, isHazardous := hazardSet[node.ptr]; isHazardous {
			remaining = append(remaining, node)
		} else {
			if node.reclaim != nil {
				node.reclaim(node.ptr)
			}
		}
	}
	hd.retired.list = remaining
	hd.retired.count = len(remaining)
}

func AcquireHazard() *HazardGuard {
	return globalDomain.Acquire()
}

func RetireHazardous(ptr unsafe.Pointer, reclaim func(unsafe.Pointer)) {
	globalDomain.Retire(ptr, reclaim)
}

type SafeQueue struct {
	vb      *VersionedRingBuffer
	domain  *HazardDomain
}

func NewSafeQueue(capacity int) *SafeQueue {
	return &SafeQueue{
		vb:     NewVersioned(capacity),
		domain: GlobalDomain(),
	}
}

func (sq *SafeQueue) Enqueue(ptr unsafe.Pointer) bool {
	return sq.vb.EnqueueVersioned(ptr)
}

func (sq *SafeQueue) Dequeue() (unsafe.Pointer, bool) {
	guard := sq.domain.Acquire()
	defer guard.Release()

	ptr, ok := sq.vb.DequeueVersioned()
	if !ok {
		return nil, false
	}
	guard.Protect(ptr)
	guard.Clear()
	return ptr, true
}

func (sq *SafeQueue) EnqueueSafe(ptr unsafe.Pointer, reclaim func(unsafe.Pointer)) bool {
	ok := sq.vb.EnqueueVersioned(ptr)
	if !ok && reclaim != nil {
		reclaim(ptr)
	}
	return ok
}

func (sq *SafeQueue) DequeueSafe() (unsafe.Pointer, bool) {
	return sq.vb.DequeueVersioned()
}

func (sq *SafeQueue) RetireNode(ptr unsafe.Pointer, reclaim func(unsafe.Pointer)) {
	sq.domain.Retire(ptr, reclaim)
}

func (sq *SafeQueue) Capacity() int {
	return sq.vb.Capacity()
}

type HazardPool struct {
	mu    sync.Mutex
	pools map[int][]unsafe.Pointer
	domain *HazardDomain
}

func NewHazardPool() *HazardPool {
	return &HazardPool{
		pools: make(map[int][]unsafe.Pointer),
		domain: GlobalDomain(),
	}
}

func (hp *HazardPool) Alloc(size int) unsafe.Pointer {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	slice := hp.pools[size]
	if len(slice) > 0 {
		ptr := slice[len(slice)-1]
		hp.pools[size] = slice[:len(slice)-1]
		return ptr
	}
	buf := make([]byte, size)
	return unsafe.Pointer(&buf[0])
}

func (hp *HazardPool) Free(ptr unsafe.Pointer, size int) {
	hp.domain.Retire(ptr, func(p unsafe.Pointer) {
		hp.mu.Lock()
		defer hp.mu.Unlock()
		hp.pools[size] = append(hp.pools[size], p)
	})
}
