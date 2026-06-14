package iec61850

import (
	"errors"
	"time"
)

const (
	EtherTypeSV     uint16 = 0x88BA
	EtherTypeGOOSE  uint16 = 0x88B8

	TagApplication0  byte = 0x60
	TagApplication1  byte = 0x61
	TagSequence      byte = 0x30
	TagInteger       byte = 0x02
	TagOctetString   byte = 0x04
	TagVisibleString byte = 0x1A
	TagUtcTime       byte = 0x23
	TagBoolean       byte = 0x01
	TagBitString     byte = 0x03
	TagOID           byte = 0x06

	BerConstructed  byte = 0x20
	BerLongForm     byte = 0x80

	UtcTimeLen           = 8
	DefaultNumSamples    = 80
	DftWindowSize        = 512
)

var (
	ErrInvalidFrame       = errors.New("iec61850: invalid frame")
	ErrFrameTooShort      = errors.New("iec61850: frame too short")
	ErrInvalidEtherType   = errors.New("iec61850: not SV/GOOSE ethertype")
	ErrTagMismatch        = errors.New("iec61850: ASN.1 tag mismatch")
	ErrInvalidTLVLength   = errors.New("iec61850: invalid TLV length encoding")
	ErrUnsupportedASDU    = errors.New("iec61850: unsupported ASDU type")
	ErrSeqDataTooShort    = errors.New("iec61850: seqData buffer too short for samples")
)

type SVHeader struct {
	AppID     uint16
	Length    uint16
	Reserved1 uint16
	Reserved2 uint16
}

type SVPayload struct {
	Header    SVHeader
	ASDUCount int
	ASDUs     []SVASDU
}

type SVASDU struct {
	SvID       string
	SmpCnt     uint32
	ConfRev    uint32
	SmpSynch   uint8
	SeqData    []byte
	SmpRate    uint32
	RefrTm     time.Time
	NumSamples int
}

type Decoder struct {
	voltageScale float64
	currentScale float64
}

func NewDecoder() *Decoder {
	return &Decoder{
		voltageScale: 1.0 / 10.0,
		currentScale: 1.0 / 100.0,
	}
}

func (d *Decoder) SetScale(voltage, current float64) {
	d.voltageScale = voltage
	d.currentScale = current
}
