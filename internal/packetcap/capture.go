//go:build cgo

package packetcap

/*
#cgo CFLAGS: -O3 -march=native -Wno-unused
#cgo linux LDFLAGS: -lrt -lpthread
#cgo darwin LDFLAGS: -lrt -lpthread

#include "capture.h"
#include <stdlib.h>
*/
import "C"

import (
	"unsafe"

	"wirebonding/ultra/pkg/models"
)

const (
	DefaultBatchSize = 256
	FrameMax         = C.PCAP_FRAME_MAX
)

type CaptureStats struct {
	RxPackets   uint64
	RxBytes     uint64
	DropPackets uint64
	DropBytes   uint64
	Errors      uint64
	HWCaptured  uint64
}

type PacketCapture struct {
	h        *C.struct_capture_handle
	ifName   string
	zeroCopy bool
	batchBuf []C.struct_frame_buf
}

func NewPacketCapture(ifName string, etherType uint16, promisc bool, zeroCopy bool) (*PacketCapture, error) {
	cIfName := C.CString(ifName)
	defer C.free(unsafe.Pointer(cIfName))
	promiscInt := 0
	if promisc {
		promiscInt = 1
	}
	zcInt := 0
	if zeroCopy {
		zcInt = 1
	}
	h := C.capture_open(cIfName, C.uint16_t(etherType), C.int(promiscInt), C.int(zcInt))
	if h == nil {
		return nil, ErrCaptureOpen
	}
	pc := &PacketCapture{
		h:        h,
		ifName:   ifName,
		zeroCopy: zeroCopy,
		batchBuf: make([]C.struct_frame_buf, DefaultBatchSize),
	}
	return pc, nil
}

func (pc *PacketCapture) ReadNext() (*models.RawFrame, error) {
	var fb C.struct_frame_buf
	n := C.capture_next_frame(pc.h, &fb)
	if n <= 0 {
		return nil, ErrFrameRead
	}
	return frameBufToRaw(&fb, pc.ifName), nil
}

func (pc *PacketCapture) ReadBatch(dst []*models.RawFrame) int {
	batchSize := len(dst)
	if batchSize > DefaultBatchSize {
		batchSize = DefaultBatchSize
	}
	n := C.capture_read_batch(pc.h, &pc.batchBuf[0], C.int(batchSize))
	if n <= 0 {
		return 0
	}
	count := int(n)
	if count > len(dst) {
		count = len(dst)
	}
	for i := 0; i < count; i++ {
		dst[i] = frameBufToRaw(&pc.batchBuf[i], pc.ifName)
	}
	return count
}

func (pc *PacketCapture) SimulateBatch(dst []*models.RawFrame, samplesPerFrame int, baseSmpcnt uint32) int {
	batchSize := len(dst)
	if batchSize > DefaultBatchSize {
		batchSize = DefaultBatchSize
	}
	n := C.capture_simulate(pc.h, &pc.batchBuf[0], C.int(batchSize),
		C.int(samplesPerFrame), C.uint32_t(baseSmpcnt))
	if n <= 0 {
		return 0
	}
	count := int(n)
	if count > len(dst) {
		count = len(dst)
	}
	for i := 0; i < count; i++ {
		dst[i] = frameBufToRaw(&pc.batchBuf[i], pc.ifName)
	}
	return count
}

func (pc *PacketCapture) Stats() CaptureStats {
	var cs C.struct_capture_stats
	C.capture_get_stats(pc.h, &cs)
	return CaptureStats{
		RxPackets:   uint64(cs.rx_packets),
		RxBytes:     uint64(cs.rx_bytes),
		DropPackets: uint64(cs.drop_packets),
		DropBytes:   uint64(cs.drop_bytes),
		Errors:      uint64(cs.errors),
		HWCaptured:  uint64(cs.hw_captured),
	}
}

func (pc *PacketCapture) Close() {
	if pc.h != nil {
		C.capture_close(pc.h)
		pc.h = nil
	}
}

func frameBufToRaw(fb *C.struct_frame_buf, ifName string) *models.RawFrame {
	length := int(fb.length)
	if length > FrameMax {
		length = FrameMax
	}
	payload := make([]byte, length)
	C.memcpy(unsafe.Pointer(&payload[0]),
		unsafe.Pointer(&fb.data[0]),
		C.size_t(length))
	rf := &models.RawFrame{
		RxTimeNs:  uint64(fb.rx_time_ns),
		Payload:   payload,
		Length:    length,
		Interface: ifName,
	}
	return rf
}
