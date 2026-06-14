package iec61850

import (
	"encoding/binary"
	"testing"

	"wirebonding/ultra/pkg/models"
)

func berLen(n int) ([]byte, int) {
	if n < 0x80 {
		return []byte{byte(n)}, 1
	} else if n <= 0xFF {
		return []byte{0x81, byte(n)}, 2
	} else {
		return []byte{0x82, byte(n >> 8), byte(n & 0xFF)}, 3
	}
}

func appendTLV(buf []byte, tag byte, data []byte) []byte {
	buf = append(buf, tag)
	lb, _ := berLen(len(data))
	buf = append(buf, lb...)
	buf = append(buf, data...)
	return buf
}

func appendIntTLV(buf []byte, tag byte, val uint32, sz int) []byte {
	if sz <= 0 {
		sz = 4
	}
	data := make([]byte, sz)
	for i := 0; i < sz; i++ {
		data[i] = byte(val >> uint((sz-1-i)*8))
	}
	return appendTLV(buf, tag, data)
}

func makeTestSVFrame(numSamples int, smpcnt uint32) *models.RawFrame {
	var seqData []byte
	for i := 0; i < numSamples; i++ {
		ph := float64(i) * 0.1
		cur := int32(20470.0 * sinApprox(ph))
		vol := int32(5110.0 * sinApprox(ph+1.5708))
		var b8 [8]byte
		binary.BigEndian.PutUint32(b8[0:], uint32(cur))
		binary.BigEndian.PutUint32(b8[4:], uint32(vol))
		seqData = append(seqData, b8[:]...)
	}

	svID := []byte("WBBOND01")
	var asduData []byte
	asduData = appendTLV(asduData, 0x80, svID)
	asduData = appendIntTLV(asduData, 0x82, smpcnt, 4)
	asduData = appendIntTLV(asduData, 0x83, 1, 4)
	asduData = appendIntTLV(asduData, 0x85, 1, 1)
	asduData = appendTLV(asduData, 0x87, seqData)

	asduTLV := appendTLV(nil, 0x30, asduData)
	seqASDU := asduTLV

	noASDU := []byte{0x01}

	var apduInner []byte
	apduInner = appendTLV(apduInner, 0x80, noASDU)
	apduInner = appendTLV(apduInner, 0xA2, seqASDU)

	apdu := appendTLV(nil, 0x60, apduInner)

	totalLen := len(apdu)
	svHeader := make([]byte, 8)
	binary.BigEndian.PutUint16(svHeader[0:], 0x0001)
	binary.BigEndian.PutUint16(svHeader[2:], uint16(totalLen))
	binary.BigEndian.PutUint16(svHeader[4:], 0)
	binary.BigEndian.PutUint16(svHeader[6:], 0)

	var eth []byte
	for i := 0; i < 6; i++ {
		eth = append(eth, 0x01)
	}
	for i := 0; i < 6; i++ {
		eth = append(eth, 0x02)
	}
	var et [2]byte
	binary.BigEndian.PutUint16(et[:], EtherTypeSV)
	eth = append(eth, et[:]...)
	eth = append(eth, svHeader...)
	eth = append(eth, apdu...)

	rf := &models.RawFrame{
		RxTimeNs:  1_000_000_000,
		Payload:   eth,
		Length:    len(eth),
		EtherType: EtherTypeSV,
	}
	for i := 0; i < 6; i++ {
		rf.DstMAC[i] = 0x01
		rf.SrcMAC[i] = 0x02
	}
	return rf
}

func sinApprox(x float64) float64 {
	const pi = 3.141592653589793
	for x > pi {
		x -= 2 * pi
	}
	for x < -pi {
		x += 2 * pi
	}
	x2 := x * x
	return x * (1.0 - x2/6.0*(1.0 - x2/20.0*(1.0 - x2/42.0)))
}

func TestParseEthernetHeader(t *testing.T) {
	rf := makeTestSVFrame(80, 0)
	off, err := ParseEthernetHeader(rf)
	if err != nil {
		t.Fatalf("ParseEthernetHeader failed: %v", err)
	}
	if off != 14 {
		t.Errorf("expected offset 14, got %d", off)
	}
	if rf.EtherType != EtherTypeSV {
		t.Errorf("expected EtherType SV %04X, got %04X", EtherTypeSV, rf.EtherType)
	}
}

func TestDecodeFull(t *testing.T) {
	rf := makeTestSVFrame(80, 12345)
	dec := NewDecoder()
	parsed, samples, err := dec.DecodeFull(rf)
	if err != nil {
		t.Fatalf("DecodeFull failed: %v (frameLen=%d payload=%v)", err, rf.Length, rf.Length > 50)
	}
	if parsed == nil {
		t.Fatal("parsed ASDU is nil")
	}
	if len(samples) != 80 {
		t.Fatalf("expected 80 samples, got %d", len(samples))
	}
	if samples[0].SeqNo != 12345 {
		t.Errorf("expected first sample SeqNo 12345, got %d", samples[0].SeqNo)
	}
	if samples[79].SeqNo != 12345+79 {
		t.Errorf("expected last sample SeqNo %d, got %d", 12345+79, samples[79].SeqNo)
	}
	_ = parsed
}

func TestExtractSamples(t *testing.T) {
	rf := makeTestSVFrame(100, 0)
	dec := NewDecoder()
	dec.SetScale(1.0, 1.0)
	_, samples, err := dec.DecodeFull(rf)
	if err != nil {
		t.Fatalf("DecodeFull failed: %v", err)
	}
	if len(samples) != 100 {
		t.Fatalf("expected 100 samples, got %d", len(samples))
	}
	for i, s := range samples {
		if s.TimestampNs == 0 {
			t.Errorf("sample %d has zero timestamp", i)
		}
	}
}

func BenchmarkDecodeFull(b *testing.B) {
	rf := makeTestSVFrame(80, 0)
	dec := NewDecoder()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, samples, err := dec.DecodeFull(rf)
		if err != nil {
			b.Fatal(err)
		}
		_ = samples
	}
}

func BenchmarkExtractSamples(b *testing.B) {
	rf := makeTestSVFrame(80, 0)
	dec := NewDecoder()
	_, _, err := dec.DecodeFull(rf)
	if err != nil {
		b.Fatal(err)
	}
	asdu := &SVASDU{
		SmpCnt:     0,
		SeqData:    make([]byte, 80*8),
		NumSamples: 80,
	}
	for i := 0; i < 80*8; i += 8 {
		copy(asdu.SeqData[i:], make([]byte, 8))
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := dec.ExtractSamples(asdu, 0)
		_ = s
	}
}
