package bridge

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrShortFrame reports a binary frame too small to hold its header.
var ErrShortFrame = errors.New("bridge: binary frame shorter than header")

// ErrFrameVersion reports a header whose version byte we don't speak.
var ErrFrameVersion = errors.New("bridge: unknown frame version")

// AppendFrame encodes f into buf and returns the extended slice. Callers
// reuse buf across frames to keep the send path allocation-free.
func AppendFrame(buf []byte, f *MediaFrame) []byte {
	var hdr [FrameHeaderLen]byte
	hdr[0] = FrameVersion
	hdr[1] = f.Kind
	if f.KeyFrame {
		hdr[2] = FlagKeyFrame
	}
	binary.BigEndian.PutUint32(hdr[4:8], f.Handle)
	binary.BigEndian.PutUint64(hdr[8:16], f.Timestamp)
	binary.BigEndian.PutUint32(hdr[16:20], uint32(len(f.Config)))
	binary.BigEndian.PutUint32(hdr[20:24], uint32(len(f.Payload)))

	buf = append(buf, hdr[:]...)
	buf = append(buf, f.Config...)
	buf = append(buf, f.Payload...)
	return buf
}

// ParseFrame decodes a binary bridge frame. The returned Config and
// Payload alias b, so a caller that retains them past the read loop must
// copy.
func ParseFrame(b []byte) (MediaFrame, error) {
	if len(b) < FrameHeaderLen {
		return MediaFrame{}, ErrShortFrame
	}
	if b[0] != FrameVersion {
		return MediaFrame{}, fmt.Errorf("%w: %d", ErrFrameVersion, b[0])
	}
	configLen := binary.BigEndian.Uint32(b[16:20])
	payloadLen := binary.BigEndian.Uint32(b[20:24])
	// Compare in uint64 so a hostile length pair cannot overflow into a
	// value that passes the bounds check.
	if uint64(FrameHeaderLen)+uint64(configLen)+uint64(payloadLen) > uint64(len(b)) {
		return MediaFrame{}, fmt.Errorf(
			"bridge: frame declares %d+%d bytes but holds %d",
			configLen, payloadLen, len(b)-FrameHeaderLen)
	}
	configEnd := FrameHeaderLen + int(configLen)
	f := MediaFrame{
		Kind:      b[1],
		Handle:    binary.BigEndian.Uint32(b[4:8]),
		Timestamp: binary.BigEndian.Uint64(b[8:16]),
		KeyFrame:  b[2]&FlagKeyFrame != 0,
		Payload:   b[configEnd : configEnd+int(payloadLen)],
	}
	if configLen > 0 {
		f.Config = b[FrameHeaderLen:configEnd]
	}
	return f, nil
}
