package ringbuf

import (
	"fmt"
	"sync/atomic"
	"unsafe"
)

const (
	cacheLineSize = 64
)

type cell[T any] struct {
	sequence atomic.Uint64
	value    T
}

type TaggedRingBuffer[T any] struct {
	padding0  [cacheLineSize]uint8
	mask      uint64
	cells     []cell[T]
	padding1  [cacheLineSize]uint8
	enqPos    atomic.Uint64
	padding2  [cacheLineSize]uint8
	deqPos    atomic.Uint64
	padding3  [cacheLineSize]uint8
}

func NewTagged[T any](capacity int) *TaggedRingBuffer[T] {
	sz := roundUpPow2(uint64(capacity))
	if sz < 2 {
		sz = 2
	}
	rb := &TaggedRingBuffer[T]{
		mask:  sz - 1,
		cells: make([]cell[T], sz),
	}
	for i := range rb.cells {
		rb.cells[i].sequence.Store(uint64(i))
	}
	return rb
}

func (rb *TaggedRingBuffer[T]) Enqueue(val T) bool {
	var cell *cell[T]
	pos := rb.enqPos.Load()
	for {
		cell = &rb.cells[pos&rb.mask]
		seq := cell.sequence.Load()
		diff := int64(seq) - int64(pos)
		if diff == 0 {
			if rb.enqPos.CompareAndSwap(pos, pos+1) {
				break
			}
		} else if diff < 0 {
			return false
		} else {
			pos = rb.enqPos.Load()
		}
	}
	cell.value = val
	cell.sequence.Store(pos + 1)
	return true
}

func (rb *TaggedRingBuffer[T]) Dequeue() (T, bool) {
	var cell *cell[T]
	pos := rb.deqPos.Load()
	for {
		cell = &rb.cells[pos&rb.mask]
		seq := cell.sequence.Load()
		diff := int64(seq) - int64(pos+1)
		if diff == 0 {
			if rb.deqPos.CompareAndSwap(pos, pos+1) {
				break
			}
		} else if diff < 0 {
			var zero T
			return zero, false
		} else {
			pos = rb.deqPos.Load()
		}
	}
	val := cell.value
	var zero T
	cell.value = zero
	cell.sequence.Store(pos + rb.mask + 1)
	return val, true
}

func (rb *TaggedRingBuffer[T]) TryEnqueue(val T) bool {
	return rb.Enqueue(val)
}

func (rb *TaggedRingBuffer[T]) TryDequeue() (T, bool) {
	return rb.Dequeue()
}

func (rb *TaggedRingBuffer[T]) Capacity() int {
	return int(rb.mask + 1)
}

func (rb *TaggedRingBuffer[T]) ApproxLen() int {
	enq := rb.enqPos.Load()
	deq := rb.deqPos.Load()
	diff := enq - deq
	if diff > rb.mask+1 {
		return int(rb.mask + 1)
	}
	return int(diff)
}

func roundUpPow2(v uint64) uint64 {
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	v++
	return v
}

type TaggedCell struct {
	Version  uint64
	Data     unsafe.Pointer
	Reserved uint64
}

const TaggedCellSize = unsafe.Sizeof(TaggedCell{})

type VersionedRingBuffer struct {
	padding0 [cacheLineSize]uint8
	cells    []unsafe.Pointer
	mask     uint64
	padding1 [cacheLineSize]uint8
	enqVer   atomic.Uint64
	enqIdx   atomic.Uint64
	padding2 [cacheLineSize]uint8
	deqVer   atomic.Uint64
	deqIdx   atomic.Uint64
	padding3 [cacheLineSize]uint8
}

func NewVersioned(capacity int) *VersionedRingBuffer {
	sz := roundUpPow2(uint64(capacity))
	if sz < 2 {
		sz = 2
	}
	vb := &VersionedRingBuffer{
		cells: make([]unsafe.Pointer, sz),
		mask:  sz - 1,
	}
	for i := range vb.cells {
		tc := &TaggedCell{Version: 1, Data: nil, Reserved: uint64(i)}
		vb.cells[i] = unsafe.Pointer(tc)
	}
	vb.enqVer.Store(1)
	vb.deqVer.Store(1)
	return vb
}

func (vb *VersionedRingBuffer) EnqueueVersioned(ptr unsafe.Pointer) bool {
	pos := vb.enqIdx.Load()
	for {
		slotIdx := pos & vb.mask
		cellPtr := atomic.LoadPointer(&vb.cells[slotIdx])
		tc := (*TaggedCell)(cellPtr)
		if tc.Version == pos+1 {
			if vb.enqIdx.CompareAndSwap(pos, pos+1) {
				newCell := &TaggedCell{
					Version: pos + 1,
					Data:    ptr,
				}
				atomic.StorePointer(&vb.cells[slotIdx], unsafe.Pointer(newCell))
				return true
			}
			continue
		}
		if int64(tc.Version) < int64(pos+1) {
			return false
		}
		pos = vb.enqIdx.Load()
	}
}

func (vb *VersionedRingBuffer) DequeueVersioned() (unsafe.Pointer, bool) {
	pos := vb.deqIdx.Load()
	for {
		slotIdx := pos & vb.mask
		cellPtr := atomic.LoadPointer(&vb.cells[slotIdx])
		tc := (*TaggedCell)(cellPtr)
		if tc.Version == pos+2 {
			if vb.deqIdx.CompareAndSwap(pos, pos+1) {
				data := tc.Data
				newCell := &TaggedCell{
					Version: pos + vb.mask + 2,
					Data:    nil,
				}
				atomic.StorePointer(&vb.cells[slotIdx], unsafe.Pointer(newCell))
				return data, true
			}
			continue
		}
		if int64(tc.Version) < int64(pos+2) {
			return nil, false
		}
		pos = vb.deqIdx.Load()
	}
}

func (vb *VersionedRingBuffer) Capacity() int {
	return int(vb.mask + 1)
}

func (vb *VersionedRingBuffer) EnqueueRaw(ptr unsafe.Pointer) bool {
	return vb.EnqueueVersioned(ptr)
}

func (vb *VersionedRingBuffer) DequeueRaw() (unsafe.Pointer, bool) {
	return vb.DequeueVersioned()
}

type BatchRingBuffer[T any] struct {
	inner *TaggedRingBuffer[T]
}

func NewBatch[T any](capacity int) *BatchRingBuffer[T] {
	return &BatchRingBuffer[T]{
		inner: NewTagged[T](capacity),
	}
}

func (b *BatchRingBuffer[T]) EnqueueBatch(vals []T) (count int) {
	for i, v := range vals {
		if !b.inner.Enqueue(v) {
			return i
		}
	}
	return len(vals)
}

func (b *BatchRingBuffer[T]) DequeueBatch(dst []T) (count int) {
	for i := range dst {
		v, ok := b.inner.Dequeue()
		if !ok {
			return i
		}
		dst[i] = v
	}
	return len(dst)
}

func (b *BatchRingBuffer[T]) TryEnqueue(val T) bool {
	return b.inner.Enqueue(val)
}

func (b *BatchRingBuffer[T]) TryDequeue() (T, bool) {
	return b.inner.Dequeue()
}

func (b *BatchRingBuffer[T]) Capacity() int {
	return b.inner.Capacity()
}

func (b *BatchRingBuffer[T]) ApproxLen() int {
	return b.inner.ApproxLen()
}

func (b *BatchRingBuffer[T]) Enqueue(val T) bool {
	return b.inner.Enqueue(val)
}

func (b *BatchRingBuffer[T]) Dequeue() (T, bool) {
	return b.inner.Dequeue()
}

func (b *BatchRingBuffer[T]) Inner() *TaggedRingBuffer[T] {
	return b.inner
}

func ValidateIntegrity[T any](rb *TaggedRingBuffer[T], rounds int) error {
	enqPos := rb.enqPos.Load()
	deqPos := rb.deqPos.Load()
	mask := rb.mask
	for i := uint64(0); i < mask+1; i++ {
		seq := rb.cells[i].sequence.Load()
		if seq < deqPos && seq > enqPos {
			return fmt.Errorf("cell[%d] sequence=%d corrupted: deqPos=%d enqPos=%d",
				i, seq, deqPos, enqPos)
		}
	}
	return nil
}
