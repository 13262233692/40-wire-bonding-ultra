package models

import (
	"time"
)

const (
	SampleRateHz     = 100_000
	SampleIntervalNs = 10_000
	MaxFrameSamples  = 4096
)

type WaveSample struct {
	TimestampNs uint64
	Voltage     float64
	Current     float64
	SeqNo       uint32
}

type Phasor struct {
	Real      float64
	Imag      float64
	Magnitude float64
	Angle     float64
	FreqHz    float64
}

type ImpedancePoint struct {
	TimestampNs uint64
	Resistance  float64
	Reactance   float64
	Magnitude   float64
	Phase       float64
	SeqNo       uint32
}

type RawFrame struct {
	RxTimeNs    uint64
	Payload     []byte
	Length      int
	Interface   string
	EtherType   uint16
	SrcMAC      [6]byte
	DstMAC      [6]byte
}

type ParsedASDU struct {
	TypeId       uint8
	Sqc          uint8
	Cot          uint8
	Test         bool
	Negative     bool
	Addr         uint32
	DataElements []DataElement
	SeqNo        uint32
}

type DataElement struct {
	Timestamp  time.Time
	Quality    uint16
	Voltage    float64
	Current    float64
	StatusBits uint8
}

type PhasorFrame struct {
	SeqNo        uint32
	TimestampNs  uint64
	VoltagePhasor Phasor
	CurrentPhasor Phasor
	Impedance    ImpedancePoint
	Samples      []WaveSample
}
