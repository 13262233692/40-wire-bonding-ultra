package canbus

import (
	"encoding/binary"
	"time"
)

const (
	CanFrameMaxDataLen = 8

	PriorityEmergency = 0x00
	PriorityHigh      = 0x01
	PriorityNormal    = 0x02
	PriorityLow       = 0x03

	CmdBondingEmergencyStop = 0x01
	CmdBondingPause         = 0x02
	CmdBondingResume        = 0x03
	CmdQualityAck           = 0x0A
	CmdQualityFailStop      = 0x0B
	CmdHeartbeat            = 0x7F

	ReasonPullStrengthBelowRedline = 0x11
	ReasonImpedanceAnomaly         = 0x12
	ReasonPhaseShiftCritical       = 0x13
	ReasonWatchdogTimeout          = 0x14

	WireBondingDeviceType = 0x08

	CanIDMaskPriority = 0xC0000000
	CanIDMaskDevice   = 0x3F000000
	CanIDMaskCmd      = 0x00FF0000
	CanIDMaskReason   = 0x0000FF00
	CanIDMaskSeq      = 0x000000FF

	RedlineMagicCookie = 0x5A5A

	FuseCRC16Poly = 0xA001
)

type Frame20B struct {
	ID         uint32
	Extended   bool
	RTR        bool
	DLC        uint8
	Data       [8]byte
	TimestampNs uint64
}

type FuseStopPayload struct {
	SeqNo         uint32
	TimestampNs   uint64
	BondID        uint16
	Reason        uint8
	PullGrams     float32
	DeviationPct  float32
	EmergencyStop bool
}

type FuseChannel struct {
	deviceID uint8
	seq      uint8
	sendCB   func(Frame20B)
}

func NewFuseChannel(deviceID uint8, sendCB func(Frame20B)) *FuseChannel {
	return &FuseChannel{
		deviceID: deviceID & 0x3F,
		sendCB:   sendCB,
	}
}

func (fc *FuseChannel) ComposeEmergencyStop(bondID uint16, reason uint8,
	pullGrams, devPct float32, seqNo uint32, tsNs uint64) Frame20B {
	fc.seq++
	id := PackCanID(PriorityEmergency, fc.deviceID, CmdBondingEmergencyStop, reason, fc.seq)
	return fc.buildFrame(id, bondID, reason, pullGrams, devPct, seqNo, tsNs, true)
}

func (fc *FuseChannel) ComposeQualityFail(bondID uint16, reason uint8,
	pullGrams, devPct float32, seqNo uint32, tsNs uint64) Frame20B {
	fc.seq++
	id := PackCanID(PriorityHigh, fc.deviceID, CmdQualityFailStop, reason, fc.seq)
	return fc.buildFrame(id, bondID, reason, pullGrams, devPct, seqNo, tsNs, false)
}

func (fc *FuseChannel) Send(frame Frame20B) {
	if fc.sendCB != nil {
		fc.sendCB(frame)
	}
}

func (fc *FuseChannel) buildFrame(id uint32, bondID uint16, reason uint8,
	pullGrams, devPct float32, seqNo uint32, tsNs uint64, emergency bool) Frame20B {

	frame := Frame20B{
		ID:          id,
		Extended:    true,
		RTR:         false,
		DLC:         8,
		TimestampNs: tsNs,
	}

	binary.LittleEndian.PutUint16(frame.Data[0:2], bondID)
	frame.Data[2] = reason
	if emergency {
		frame.Data[3] = 0x01
	} else {
		frame.Data[3] = 0x00
	}
	pg := uint16(pullGrams * 100.0)
	binary.LittleEndian.PutUint16(frame.Data[4:6], pg)
	dp := uint16(devPct * 100.0)
	binary.LittleEndian.PutUint16(frame.Data[6:8], dp)
	return frame
}

func PackCanID(priority, device, cmd, reason, seq uint8) uint32 {
	var id uint32
	id |= (uint32(priority) & 0x03) << 30
	id |= (uint32(device) & 0x3F) << 24
	id |= (uint32(cmd) & 0xFF) << 16
	id |= (uint32(reason) & 0xFF) << 8
	id |= (uint32(seq) & 0xFF)
	return id
}

func UnpackCanID(id uint32) (priority, device, cmd, reason, seq uint8) {
	priority = uint8((id >> 30) & 0x03)
	device = uint8((id >> 24) & 0x3F)
	cmd = uint8((id >> 16) & 0xFF)
	reason = uint8((id >> 8) & 0xFF)
	seq = uint8(id & 0xFF)
	return
}

func (f Frame20B) PayloadDecode() FuseStopPayload {
	p := FuseStopPayload{}
	if f.DLC < 8 {
		return p
	}
	p.BondID = binary.LittleEndian.Uint16(f.Data[0:2])
	p.Reason = f.Data[2]
	p.EmergencyStop = f.Data[3] != 0
	pgRaw := binary.LittleEndian.Uint16(f.Data[4:6])
	p.PullGrams = float32(pgRaw) / 100.0
	dpRaw := binary.LittleEndian.Uint16(f.Data[6:8])
	p.DeviationPct = float32(dpRaw) / 100.0
	p.TimestampNs = f.TimestampNs
	_, _, _, _, sq := UnpackCanID(f.ID)
	p.SeqNo = uint32(sq)
	return p
}

func CRC16Fuse(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&0x0001 != 0 {
				crc = (crc >> 1) ^ FuseCRC16Poly
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

type FuseStats struct {
	EmergencyFrames uint64
	QualityFrames   uint64
	LastFrame       Frame20B
	LastFrameTime   time.Time
}

type FuseDispatcher struct {
	channels map[uint8]*FuseChannel
	stats    FuseStats
	sendAll  func(Frame20B)
}

func NewFuseDispatcher(sendAll func(Frame20B)) *FuseDispatcher {
	return &FuseDispatcher{
		channels: make(map[uint8]*FuseChannel),
		sendAll:  sendAll,
	}
}

func (fd *FuseDispatcher) GetChannel(deviceID uint8) *FuseChannel {
	ch, ok := fd.channels[deviceID]
	if !ok {
		cb := func(f Frame20B) {
			if fd.sendAll != nil {
				fd.sendAll(f)
			}
			fd.stats.LastFrame = f
			fd.stats.LastFrameTime = time.Now()
			_, _, cmd, _, _ := UnpackCanID(f.ID)
			if cmd == CmdBondingEmergencyStop {
				fd.stats.EmergencyFrames++
			} else if cmd == CmdQualityFailStop {
				fd.stats.QualityFrames++
			}
		}
		ch = NewFuseChannel(deviceID, cb)
		fd.channels[deviceID] = ch
	}
	return ch
}

func (fd *FuseDispatcher) Stats() FuseStats { return fd.stats }

func (fd *FuseDispatcher) DispatchEmergency(bondID uint16, reason uint8,
	pullGrams, devPct float32, seqNo uint32, tsNs uint64, deviceID uint8) Frame20B {
	ch := fd.GetChannel(deviceID)
	frame := ch.ComposeEmergencyStop(bondID, reason, pullGrams, devPct, seqNo, tsNs)
	ch.Send(frame)
	return frame
}
