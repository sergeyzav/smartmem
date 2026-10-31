package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

var (
	// ErrNoTerminator means a string has no terminator within its maximum length.
	ErrNoTerminator = errors.New("smartmem: string terminator not found")
	// ErrNullBuffer means a UNICODE_STRING has a length but a null Buffer.
	ErrNullBuffer = errors.New("smartmem: string buffer pointer is null")
)

// DecodeCString returns the bytes of b before the first NUL. Without a NUL it
// returns ErrNoTerminator, or all of b if truncate is set.
func DecodeCString(b []byte, truncate bool) (string, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i]), nil
	}
	if truncate {
		return string(b), nil
	}
	return "", ErrNoTerminator
}

// DecodeUTF16 decodes little-endian UTF-16 up to the first 0x0000 unit. A
// trailing odd byte is ignored. Without a terminator it returns
// ErrNoTerminator, or all of b if truncate is set. Invalid surrogates become
// U+FFFD.
func DecodeUTF16(b []byte, truncate bool) (string, error) {
	n := len(b) / 2
	for i := range n {
		if b[2*i] == 0 && b[2*i+1] == 0 {
			return DecodeUTF16Exact(b[:2*i]), nil
		}
	}
	if truncate {
		return DecodeUTF16Exact(b), nil
	}
	return "", ErrNoTerminator
}

// DecodeUTF16Exact decodes all of b as little-endian UTF-16, without looking
// for a terminator. A trailing odd byte is ignored.
func DecodeUTF16Exact(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}

// UnicodeStringHeader mirrors the x64 UNICODE_STRING struct.
type UnicodeStringHeader struct {
	Length        uint16 // in bytes, without a terminator
	MaximumLength uint16
	_             [4]byte
	Buffer        uint64
}

// UnicodeRange returns where the characters of a UNICODE_STRING are and how
// many bytes to read. An odd Length is rounded down, and a Length above
// maxLen is cut to maxLen. n == 0 means an empty string: nothing to read.
func UnicodeRange(h UnicodeStringHeader, maxLen int) (addr uint64, n int, err error) {
	n = int(h.Length) &^ 1
	if n == 0 {
		return 0, 0, nil
	}
	if h.Buffer == 0 {
		return 0, 0, ErrNullBuffer
	}
	return h.Buffer, min(n, maxLen&^1), nil
}
