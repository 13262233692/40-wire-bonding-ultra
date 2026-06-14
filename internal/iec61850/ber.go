package iec61850

import "time"

func readBerTag(buf []byte, offset int) (tag byte, newOffset int, err error) {
	if offset >= len(buf) {
		return 0, offset, ErrFrameTooShort
	}
	tag = buf[offset]
	return tag, offset + 1, nil
}

func readBerLength(buf []byte, offset int) (length int, newOffset int, err error) {
	if offset >= len(buf) {
		return 0, offset, ErrFrameTooShort
	}
	b := buf[offset]
	offset++
	if b < BerLongForm {
		return int(b), offset, nil
	}
	numBytes := int(b & 0x7F)
	if numBytes == 0 || numBytes > 4 {
		return 0, offset, ErrInvalidTLVLength
	}
	if offset+numBytes > len(buf) {
		return 0, offset, ErrFrameTooShort
	}
	length = 0
	for i := 0; i < numBytes; i++ {
		length = (length << 8) | int(buf[offset+i])
	}
	return length, offset + numBytes, nil
}

func readTLV(buf []byte, offset int) (tag byte, data []byte, newOffset int, err error) {
	tag, offset, err = readBerTag(buf, offset)
	if err != nil {
		return 0, nil, offset, err
	}
	length, offset, err := readBerLength(buf, offset)
	if err != nil {
		return 0, nil, offset, err
	}
	if length < 0 || offset+length > len(buf) {
		return 0, nil, offset, ErrFrameTooShort
	}
	data = buf[offset : offset+length]
	return tag, data, offset + length, nil
}

func expectTag(buf []byte, offset int, expected byte) (data []byte, newOffset int, err error) {
	tag, data, offset, err := readTLV(buf, offset)
	if err != nil {
		return nil, offset, err
	}
	if tag != expected {
		return nil, offset, ErrTagMismatch
	}
	return data, offset, nil
}

func decodeInteger32(buf []byte) (uint32, error) {
	if len(buf) == 0 || len(buf) > 4 {
		return 0, ErrInvalidTLVLength
	}
	var v uint32
	if buf[0]&0x80 != 0 {
		v = 0xFFFFFFFF
	}
	for _, b := range buf {
		v = (v << 8) | uint32(b)
	}
	return v, nil
}

func decodeIntegerSigned32(buf []byte) (int32, error) {
	if len(buf) == 0 || len(buf) > 4 {
		return 0, ErrInvalidTLVLength
	}
	var v int32
	if buf[0]&0x80 != 0 {
		v = -1
	}
	for _, b := range buf {
		v = (v << 8) | int32(b)
	}
	return v, nil
}

func decodeVisibleString(data []byte) string {
	return string(data)
}

func decodeUtcTime(data []byte) (time.Time, error) {
	if len(data) < 8 {
		return time.Time{}, ErrFrameTooShort
	}
	secondsSinceEpoch := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	fraction := uint32(data[4])<<16 | uint32(data[5])<<8 | uint32(data[6])
	t := time.Unix(int64(secondsSinceEpoch), int64(fraction)*1000)
	return t, nil
}

func decodeBitString(data []byte) (uint16, error) {
	if len(data) < 2 {
		return 0, ErrFrameTooShort
	}
	unusedBits := int(data[0])
	_ = unusedBits
	val := uint16(0)
	for i := 1; i < len(data) && i-1 < 2; i++ {
		val = (val << 8) | uint16(data[i])
	}
	return val, nil
}
