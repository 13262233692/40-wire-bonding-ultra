package canbus

import (
	"testing"
)

func TestCanID_PackUnpack(t *testing.T) {
	id := PackCanID(PriorityEmergency, 0x01, CmdBondingEmergencyStop, ReasonPullStrengthBelowRedline, 0x42)
	pri, dev, cmd, reason, seq := UnpackCanID(id)
	if pri != PriorityEmergency {
		t.Errorf("priority = %d want %d", pri, PriorityEmergency)
	}
	if dev != 0x01 {
		t.Errorf("device = %d want 1", dev)
	}
	if cmd != CmdBondingEmergencyStop {
		t.Errorf("cmd = 0x%02X want 0x%02X", cmd, CmdBondingEmergencyStop)
	}
	if reason != ReasonPullStrengthBelowRedline {
		t.Errorf("reason = 0x%02X want 0x%02X", reason, ReasonPullStrengthBelowRedline)
	}
	if seq != 0x42 {
		t.Errorf("seq = %d want 66", seq)
	}
}

func TestFuseChannel_EmergencyStop(t *testing.T) {
	var last Frame20B
	ch := NewFuseChannel(0x01, func(f Frame20B) {
		last = f
	})

	bondID := uint16(42)
	pullG := float32(3.12)
	devPct := float32(-37.6)
	seqNo := uint32(12345)
	tsNs := uint64(9876543210)

	frame := ch.ComposeEmergencyStop(bondID, ReasonPullStrengthBelowRedline, pullG, devPct, seqNo, tsNs)
	ch.Send(frame)

	if last.ID != frame.ID {
		t.Fatal("frame not sent")
	}
	if !last.Extended {
		t.Error("should be extended frame")
	}
	if last.DLC != 8 {
		t.Errorf("DLC = %d want 8", last.DLC)
	}

	pl := frame.PayloadDecode()
	if pl.BondID != bondID {
		t.Errorf("bondID = %d want %d", pl.BondID, bondID)
	}
	if pl.Reason != ReasonPullStrengthBelowRedline {
		t.Errorf("reason = 0x%02X", pl.Reason)
	}
	if !pl.EmergencyStop {
		t.Error("emergency flag not set")
	}
	if abs(float64(pl.PullGrams)-float64(pullG)) > 0.02 {
		t.Errorf("pullGrams = %.2f want %.2f", pl.PullGrams, pullG)
	}
}

func TestFuseChannel_QualityFail(t *testing.T) {
	ch := NewFuseChannel(0x02, func(f Frame20B) {})
	frame := ch.ComposeQualityFail(100, ReasonImpedanceAnomaly, 4.5, -10.0, 99, 12345)
	_, _, cmd, _, _ := UnpackCanID(frame.ID)
	if cmd != CmdQualityFailStop {
		t.Errorf("cmd = 0x%02X want 0x%02X", cmd, CmdQualityFailStop)
	}
	pl := frame.PayloadDecode()
	if pl.EmergencyStop {
		t.Error("quality fail should not set emergency flag")
	}
}

func TestCRC16Fuse(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	crc := CRC16Fuse(data)
	if crc == 0 || crc == 0xFFFF {
		t.Errorf("bad CRC = 0x%04X", crc)
	}
}

func TestFuseDispatcher_Dispatch(t *testing.T) {
	var sent Frame20B
	var count int
	fd := NewFuseDispatcher(func(f Frame20B) {
		sent = f
		count++
	})

	frame := fd.DispatchEmergency(100, ReasonPullStrengthBelowRedline, 3.0, -40.0, 500, 1000, 0x05)
	if count != 1 {
		t.Fatalf("dispatcher send count = %d want 1", count)
	}
	if sent.ID != frame.ID {
		t.Fatal("frame mismatch")
	}
	stats := fd.Stats()
	if stats.EmergencyFrames != 1 {
		t.Errorf("emergency frames = %d want 1", stats.EmergencyFrames)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
