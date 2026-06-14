//go:build !cgo

package packetcap

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"wirebonding/ultra/pkg/models"
)

const (
	DefaultBatchSize = 256
	FrameMax         = 9216
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
	mu         sync.Mutex
	ifName     string
	zeroCopy   bool
	etherType  uint16
	promisc    bool
	stats      CaptureStats
	started    bool
	baseSmpcnt uint32
	phaseAcc   uint32
	seq        atomic.Uint64
}

func NewPacketCapture(ifName string, etherType uint16, promisc bool, zeroCopy bool) (*PacketCapture, error) {
	return &PacketCapture{
		ifName:    ifName,
		zeroCopy:  zeroCopy,
		etherType: etherType,
		promisc:   promisc,
	}, nil
}

func (pc *PacketCapture) ReadNext() (*models.RawFrame, error) {
	batch := make([]*models.RawFrame, 1)
	n := pc.SimulateBatch(batch, 80, pc.baseSmpcnt)
	if n <= 0 {
		return nil, ErrFrameRead
	}
	return batch[0], nil
}

func (pc *PacketCapture) ReadBatch(dst []*models.RawFrame) int {
	return pc.SimulateBatch(dst, 80, pc.baseSmpcnt)
}

func (pc *PacketCapture) SimulateBatch(dst []*models.RawFrame, samplesPerFrame int, baseSmpcnt uint32) int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if samplesPerFrame <= 0 {
		samplesPerFrame = 80
	}
	n := len(dst)
	now := uint64(time.Now().UnixNano())
	cur := baseSmpcnt
	intervalNs := uint64(models.SampleIntervalNs)
	for i := 0; i < n; i++ {
		rf := buildSVFramePureGo(cur, samplesPerFrame, pc.phaseAcc, now+uint64(i)*uint64(samplesPerFrame)*intervalNs)
		rf.Interface = pc.ifName
		dst[i] = rf
		cur += uint32(samplesPerFrame)
		pc.phaseAcc += uint32(samplesPerFrame * 169)
		pc.stats.RxPackets++
		pc.stats.RxBytes += uint64(rf.Length)
	}
	pc.baseSmpcnt = cur
	return n
}

func (pc *PacketCapture) Stats() CaptureStats {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.stats
}

func (pc *PacketCapture) Close() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.started = false
}

func buildSVFramePureGo(smpcnt uint32, numSamples int, phaseAcc uint32, rxNs uint64) *models.RawFrame {
	svid := "WBBOND01"
	asdu := []byte{}
	{
		asdu = append(asdu, 0x80, byte(len(svid)))
		asdu = append(asdu, svid...)
		asdu = append(asdu, 0x82, 0x04)
		asdu = append(asdu, byte(smpcnt>>24), byte(smpcnt>>16), byte(smpcnt>>8), byte(smpcnt))
		asdu = append(asdu, 0x83, 0x04, 0x00, 0x00, 0x00, 0x01)
		asdu = append(asdu, 0x85, 0x01, 0x01)
		seqData := make([]byte, 0, numSamples*8)
		for i := 0; i < numSamples; i++ {
			ph := float64(phaseAcc + uint32(i*169))
			phaseRad := ph * math.Pi / 32768.0
			current := int32(20470.0 * math.Sin(phaseRad))
			voltage := int32(5110.0 * math.Sin(phaseRad+math.Pi/2))
			seqData = append(seqData,
				byte(current>>24), byte(current>>16), byte(current>>8), byte(current),
				byte(voltage>>24), byte(voltage>>16), byte(voltage>>8), byte(voltage))
		}
		asdu = append(asdu, 0x87)
		lb, _ := berTLVLen(len(seqData))
		asdu = append(asdu, lb...)
		asdu = append(asdu, seqData...)
	}
	seqASDU := append([]byte{0x30}, berLenBytes(len(asdu))...)
	seqASDU = append(seqASDU, asdu...)
	apduInner := append([]byte{0x80, 0x01, 0x01}, 0xA2)
	apduInner = append(apduInner, berLenBytes(len(seqASDU))...)
	apduInner = append(apduInner, seqASDU...)
	apdu := append([]byte{0x60}, berLenBytes(len(apduInner))...)
	apdu = append(apdu, apduInner...)

	svHdrLenPos := 14 + 2
	buf := make([]byte, 0, 14+8+len(apdu))
	for i := 0; i < 6; i++ {
		buf = append(buf, 0x01)
	}
	for i := 0; i < 6; i++ {
		buf = append(buf, 0x02)
	}
	buf = append(buf, byte(iecTypeSV>>8), byte(iecTypeSV&0xFF))
	buf = append(buf, 0x00, 0x01)
	apduLenBytes := []byte{byte(len(apdu) >> 8), byte(len(apdu))}
	buf = append(buf, apduLenBytes...)
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)
	buf = append(buf, apdu...)

	rf := &models.RawFrame{
		RxTimeNs:  rxNs,
		Payload:   buf,
		Length:    len(buf),
		EtherType: iecTypeSV,
	}
	for i := 0; i < 6; i++ {
		rf.DstMAC[i] = 0x01
		rf.SrcMAC[i] = 0x02
	}
	_ = svHdrLenPos
	return rf
}

func berTLVLen(n int) ([]byte, int) {
	if n < 0x80 {
		return []byte{byte(n)}, 1
	} else if n < 0x100 {
		return []byte{0x81, byte(n)}, 2
	} else {
		return []byte{0x82, byte(n >> 8), byte(n & 0xFF)}, 3
	}
}

func berLenBytes(n int) []byte {
	b, _ := berTLVLen(n)
	return b
}

const iecTypeSV uint16 = 0x88BA
