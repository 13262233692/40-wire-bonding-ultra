package iec61850

import (
	"encoding/binary"
	"time"

	"wirebonding/ultra/pkg/models"
)

func ParseEthernetHeader(raw *models.RawFrame) (svPayloadStart int, err error) {
	if raw.Length < 14 {
		return 0, ErrFrameTooShort
	}
	copy(raw.DstMAC[:], raw.Payload[0:6])
	copy(raw.SrcMAC[:], raw.Payload[6:12])
	raw.EtherType = binary.BigEndian.Uint16(raw.Payload[12:14])
	offset := 14
	if raw.EtherType == 0x8100 {
		if raw.Length < 18 {
			return 0, ErrFrameTooShort
		}
		raw.EtherType = binary.BigEndian.Uint16(raw.Payload[16:18])
		offset = 18
	}
	if raw.EtherType != EtherTypeSV && raw.EtherType != EtherTypeGOOSE {
		return 0, ErrInvalidEtherType
	}
	return offset, nil
}

func ParseSVHeader(payload []byte, offset int) (hdr SVHeader, newOffset int, err error) {
	if len(payload)-offset < 8 {
		return SVHeader{}, offset, ErrFrameTooShort
	}
	hdr.AppID = binary.BigEndian.Uint16(payload[offset : offset+2])
	hdr.Length = binary.BigEndian.Uint16(payload[offset+2 : offset+4])
	hdr.Reserved1 = binary.BigEndian.Uint16(payload[offset+4 : offset+6])
	hdr.Reserved2 = binary.BigEndian.Uint16(payload[offset+6 : offset+8])
	return hdr, offset + 8, nil
}

func ParseSVASDU(buf []byte) (asdu SVASDU, err error) {
	offset := 0
	bufLen := len(buf)
	if bufLen == 0 {
		return asdu, ErrSeqDataTooShort
	}

	if offset < bufLen && (buf[offset] == 0x1A || buf[offset] == 0x80) {
		tag, data, newOff, e := readTLV(buf, offset)
		if e == nil {
			if tag == 0x1A || tag == 0x80 {
				asdu.SvID = decodeVisibleString(data)
			}
			offset = newOff
		}
	}

	for offset < bufLen {
		tag, data, newOff, e := readTLV(buf, offset)
		if e != nil {
			break
		}
		offset = newOff
		switch tag {
		case 0x02, 0x82, 0x83, 0x86:
			v, decErr := decodeInteger32(data)
			if decErr != nil {
				continue
			}
			switch tag {
			case 0x82:
				asdu.SmpCnt = v
			case 0x83:
				asdu.ConfRev = v
			case 0x86:
				asdu.SmpRate = v
			default:
				if asdu.SmpCnt == 0 {
					asdu.SmpCnt = v
				} else if asdu.ConfRev == 0 {
					asdu.ConfRev = v
				} else {
					asdu.SmpRate = v
				}
			}
		case 0x04, 0x87:
			asdu.SeqData = make([]byte, len(data))
			copy(asdu.SeqData, data)
			asdu.NumSamples = len(data) / 8
		case 0x1A, 0x80, 0x81:
			if asdu.SvID == "" {
				asdu.SvID = decodeVisibleString(data)
			}
		case 0x23, 0x84:
			asdu.RefrTm, _ = decodeUtcTime(data)
		case 0x03, 0x85:
			if len(data) >= 1 {
				asdu.SmpSynch = data[len(data)-1]
			} else {
				bs, be := decodeBitString(data)
				if be == nil {
					asdu.SmpSynch = uint8(bs & 0xFF)
				}
			}
		default:
			continue
		}
	}
	return asdu, nil
}

func ParseAPDU(buf []byte, offset int) (svPayload SVPayload, newOffset int, err error) {
	apduTag, apduData, apduEndOffset, errOuter := readTLV(buf, offset)
	if errOuter != nil {
		return svPayload, offset, errOuter
	}
	_ = apduTag
	innerOffset := 0
	innerData := apduData

	asduCount := 1
	if innerOffset < len(innerData) && innerData[innerOffset] == 0x80 {
		tag, data, nextOff, errTLV := readTLV(innerData, innerOffset)
		if errTLV == nil && tag == 0x80 {
			if cv, errDec := decodeInteger32(data); errDec == nil {
				asduCount = int(cv)
			}
			innerOffset = nextOff
		}
	}

	if innerOffset < len(innerData) && (innerData[innerOffset] == 0xA2 || innerData[innerOffset] == 0x30) {
		tag, data, nextOff, errTLV := readTLV(innerData, innerOffset)
		if errTLV == nil {
			_ = tag
			innerData = data
			innerOffset = 0
		} else {
			_ = nextOff
		}
	}

	svPayload.ASDUCount = asduCount
	svPayload.ASDUs = make([]SVASDU, 0, asduCount)

	for i := 0; i < asduCount && innerOffset < len(innerData); i++ {
		var asduRaw []byte
		if innerOffset < len(innerData) && innerData[innerOffset] == 0x30 {
			tag, data, nextOff, errTLV := readTLV(innerData, innerOffset)
			if errTLV != nil {
				break
			}
			_ = tag
			asduRaw = data
			innerOffset = nextOff
		} else {
			remaining := len(innerData) - innerOffset
			guessLen := remaining / maxInt(asduCount-i, 1)
			if guessLen < 16 {
				guessLen = remaining
			}
			end := innerOffset + guessLen
			if end > len(innerData) {
				end = len(innerData)
			}
			asduRaw = innerData[innerOffset:end]
			innerOffset = end
		}
		if asdu, errParse := ParseSVASDU(asduRaw); errParse == nil {
			svPayload.ASDUs = append(svPayload.ASDUs, asdu)
		}
	}

	if len(svPayload.ASDUs) == 0 {
		fallback, errFallback := ParseSVASDU(innerData)
		if errFallback == nil && fallback.NumSamples > 0 {
			svPayload.ASDUCount = 1
			svPayload.ASDUs = append(svPayload.ASDUs, fallback)
		}
	}

	return svPayload, apduEndOffset, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (d *Decoder) ExtractSamples(asdu *SVASDU, rxTimeNs uint64) []models.WaveSample {
	seqLen := len(asdu.SeqData)
	if seqLen < 8 {
		return nil
	}
	numSamples := seqLen / 8
	if numSamples == 0 {
		return nil
	}
	samples := make([]models.WaveSample, numSamples)
	baseSeq := asdu.SmpCnt
	interval := uint64(models.SampleIntervalNs)

	for i := 0; i < numSamples; i++ {
		off := i * 8
		currentRaw := int32(binary.BigEndian.Uint32(asdu.SeqData[off : off+4]))
		voltageRaw := int32(binary.BigEndian.Uint32(asdu.SeqData[off+4 : off+8]))

		samples[i] = models.WaveSample{
			TimestampNs: rxTimeNs + uint64(i)*interval,
			Voltage:     float64(voltageRaw) * d.voltageScale,
			Current:     float64(currentRaw) * d.currentScale,
			SeqNo:       baseSeq + uint32(i),
		}
	}
	return samples
}

func (d *Decoder) DecodeFull(raw *models.RawFrame) (*models.ParsedASDU, []models.WaveSample, error) {
	ethOffset, err := ParseEthernetHeader(raw)
	if err != nil {
		return nil, nil, err
	}

	svHdr, apduOffset, err := ParseSVHeader(raw.Payload, ethOffset)
	if err != nil {
		return nil, nil, err
	}
	_ = svHdr

	svPayload, _, err := ParseAPDU(raw.Payload, apduOffset)
	if err != nil {
		return nil, nil, err
	}
	if len(svPayload.ASDUs) == 0 {
		return nil, nil, ErrUnsupportedASDU
	}

	asdu0 := &svPayload.ASDUs[0]
	parsed := &models.ParsedASDU{
		TypeId:       uint8(svHdr.AppID & 0xFF),
		Sqc:          asdu0.SmpSynch,
		Cot:          uint8(asdu0.ConfRev & 0xFF),
		Addr:         asdu0.ConfRev,
		SeqNo:        asdu0.SmpCnt,
		DataElements: nil,
	}

	var refrTime time.Time
	if !asdu0.RefrTm.IsZero() {
		refrTime = asdu0.RefrTm
	} else {
		refrTime = time.Now()
	}

	samples := d.ExtractSamples(asdu0, raw.RxTimeNs)
	parsed.DataElements = make([]models.DataElement, 0, len(samples))
	intervalNs := int64(models.SampleIntervalNs)
	for i, s := range samples {
		parsed.DataElements = append(parsed.DataElements, models.DataElement{
			Timestamp:  refrTime.Add(time.Duration(int64(i) * intervalNs)),
			Quality:    0,
			Voltage:    s.Voltage,
			Current:    s.Current,
			StatusBits: 0,
		})
	}

	return parsed, samples, nil
}
