package codec_test

import (
	"encoding/binary"
	"errors"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sergeyzav/smartmem/internal/codec"
)

func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// C4: string decoding edge cases.
func TestC4_CString(t *testing.T) {
	cases := []struct {
		name     string
		in       []byte
		truncate bool
		want     string
		err      error
	}{
		{"terminated", []byte("abc\x00def"), false, "abc", nil},
		{"terminator at the last byte", []byte("abcd\x00"), false, "abcd", nil},
		{"empty", []byte("\x00abc"), false, "", nil},
		{"no terminator", []byte("abcd"), false, "", codec.ErrNoTerminator},
		{"no terminator, truncate", []byte("abcd"), true, "abcd", nil},
	}
	for _, tc := range cases {
		got, err := codec.DecodeCString(tc.in, tc.truncate)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestC4_UTF16(t *testing.T) {
	cases := []struct {
		name     string
		in       []byte
		truncate bool
		want     string
		err      error
	}{
		{"terminated", append(utf16le("hi"), 0, 0, 'x', 0), false, "hi", nil},
		{"surrogate pair", append(utf16le("a😀b"), 0, 0), false, "a😀b", nil},
		{"lone surrogate", append(binary.LittleEndian.AppendUint16(nil, 0xd800), 0, 0), false, "\uFFFD", nil},
		{"zero byte inside a unit is not a terminator", append([]byte{'A', 0, 0, 'B'}, 0, 0), false, "A\u4200", nil},
		{"odd trailing byte ignored", append(utf16le("ok"), 'z'), true, "ok", nil},
		{"no terminator", utf16le("abc"), false, "", codec.ErrNoTerminator},
		{"no terminator, truncate", utf16le("abc"), true, "abc", nil},
	}
	for _, tc := range cases {
		got, err := codec.DecodeUTF16(tc.in, tc.truncate)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestC4_UnicodeString(t *testing.T) {
	cases := []struct {
		name   string
		h      codec.UnicodeStringHeader
		maxLen int
		addr   uint64
		n      int
		err    error
	}{
		{"normal", codec.UnicodeStringHeader{Length: 10, MaximumLength: 12, Buffer: 0x5000}, 4096, 0x5000, 10, nil},
		{"empty reads nothing", codec.UnicodeStringHeader{Length: 0, Buffer: 0x5000}, 4096, 0, 0, nil},
		{"empty with null buffer is fine", codec.UnicodeStringHeader{}, 4096, 0, 0, nil},
		{"odd length rounds down", codec.UnicodeStringHeader{Length: 7, Buffer: 0x5000}, 4096, 0x5000, 6, nil},
		{"cut to max length", codec.UnicodeStringHeader{Length: 0xfffe, Buffer: 0x5000}, 4096, 0x5000, 4096, nil},
		{"odd max length rounds down", codec.UnicodeStringHeader{Length: 100, Buffer: 0x5000}, 9, 0x5000, 8, nil},
		{"null buffer", codec.UnicodeStringHeader{Length: 4}, 4096, 0, 0, codec.ErrNullBuffer},
	}
	for _, tc := range cases {
		addr, n, err := codec.UnicodeRange(tc.h, tc.maxLen)
		if addr != tc.addr || n != tc.n || !errors.Is(err, tc.err) {
			t.Errorf("%s: got %#x, %d, %v; want %#x, %d, %v", tc.name, addr, n, err, tc.addr, tc.n, tc.err)
		}
	}
	if got := codec.DecodeUTF16Exact(utf16le("C:\\x")); got != "C:\\x" {
		t.Errorf("DecodeUTF16Exact = %q", got)
	}
}

// Z3: decoders never panic, and results stay within the input.
func FuzzZ3_DecodeString(f *testing.F) {
	f.Add([]byte("abc\x00"), true)
	f.Add(utf16le("a😀"), false)
	f.Add([]byte{0xd8, 0x00, 0xdc}, true)
	f.Add([]byte{}, false)
	f.Fuzz(func(t *testing.T, b []byte, truncate bool) {
		s, err := codec.DecodeCString(b, truncate)
		if err == nil && (len(s) > len(b) || containsNUL(s)) {
			t.Fatalf("DecodeCString(%q) = %q", b, s)
		}
		s, err = codec.DecodeUTF16(b, truncate)
		if err == nil && (!utf8.ValidString(s) || utf8.RuneCountInString(s) > len(b)/2) {
			t.Fatalf("DecodeUTF16(% x) = %q", b, s)
		}
		if s := codec.DecodeUTF16Exact(b); !utf8.ValidString(s) || utf8.RuneCountInString(s) > len(b)/2 {
			t.Fatalf("DecodeUTF16Exact(% x) = %q", b, s)
		}
	})
}

func containsNUL(s string) bool {
	for i := range len(s) {
		if s[i] == 0 {
			return true
		}
	}
	return false
}
