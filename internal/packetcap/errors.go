package packetcap

import "errors"

var (
	ErrCaptureOpen = errors.New("packetcap: failed to open capture handle")
	ErrFrameRead   = errors.New("packetcap: failed to read next frame")
	ErrBatchEmpty  = errors.New("packetcap: batch buffer empty")
)
