package execution

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Wire format: every frame is a 4-byte big-endian length followed by that many
// bytes of JSON. Length-prefixing lets the reader reject an oversized frame
// before allocating it.
//
// Message types (IDs are scoped to the side that issued the call, so the two
// directions never collide):
//
//	call   caller -> callee   starts call ID (Stream asks for events)
//	cancel caller -> callee   cancels call ID
//	event  callee -> caller   one stream event of call ID
//	reply  callee -> caller   final result or error of call ID (always last)
const (
	tCall   = "call"
	tCancel = "cancel"
	tEvent  = "event"
	tReply  = "reply"

	headerSize = 4
)

type frame struct {
	Type   string          `json:"type"`
	ID     uint64          `json:"id"`
	Method string          `json:"method,omitempty"`
	Stream bool            `json:"stream,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

func frameDataSize(f *frame) int64 {
	size := int64(len(f.Data)) + int64(len(f.Method))
	if f.Error != nil {
		size += int64(len(f.Error.Code)) + int64(len(f.Error.Message)) + int64(len(f.Error.Data))
	}
	return size
}

// encodeFrame returns header+body ready for a single Write.
func encodeFrame(f *frame, limit int64) ([]byte, error) {
	size := frameDataSize(f)
	if size > limit {
		return nil, fmt.Errorf("%w: payload %d bytes", ErrFrameTooLarge, len(f.Data))
	}
	buf := bytes.NewBuffer(make([]byte, headerSize, headerSize+128+len(f.Data)))
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	b = b[:len(b)-1] // Encode appends '\n'
	n := int64(len(b) - headerSize)
	if n > limit {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	binary.BigEndian.PutUint32(b, uint32(n))
	return b, nil
}

// readFrame reads one frame. A clean EOF between frames is io.EOF.
func readFrame(r io.Reader, limit int64) (*frame, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int64(binary.BigEndian.Uint32(hdr[:]))
	if n == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrProtocol)
	}
	if n > limit {
		return nil, fmt.Errorf("%w: announced %d bytes", ErrFrameTooLarge, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	var f frame
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON frame", ErrProtocol)
	}
	return &f, nil
}

// encodeValue marshals a call parameter. nil encodes to no payload.
func encodeValue(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
