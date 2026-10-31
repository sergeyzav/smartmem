package smartmem_test

import (
	"encoding/binary"
	"errors"
	"testing"
	"unicode/utf16"

	"github.com/sergeyzav/smartmem"
)

func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

func unicodeString(e *env, addr, buf uint64, s string) {
	data := utf16le(s)
	h := make([]byte, 16)
	binary.LittleEndian.PutUint16(h[0:], uint16(len(data)))
	binary.LittleEndian.PutUint16(h[2:], uint16(len(data)+2))
	binary.LittleEndian.PutUint64(h[8:], buf)
	e.fp.Bytes(addr, h).Bytes(buf, data)
}

func TestReadString(t *testing.T) {
	e := newEnv(t, smartmem.WithMaxStringLen(8))
	e.fp.Bytes(0x1000, []byte("player\x00junk"))
	e.fp.Bytes(0x1100, []byte("abcd"))
	e.fp.Bytes(0x1200, append(utf16le("Ünïcode"), 0, 0))
	e.fp.U64(0x1300, 0x1000)
	unicodeString(e, 0x2000, 0x3000, "C:\\x")
	unicodeString(e, 0x2100, 0x3100, "too long for eight bytes")
	unicodeString(e, 0x2200, 0, "")
	e.fp.Bytes(0x2300, []byte{8, 0, 10, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	b := e.s.NewBatch()
	c := smartmem.ReadString(b, e.p.At(0x1000), smartmem.CString(32))
	noTerm := smartmem.ReadString(b, e.p.At(0x1100), smartmem.CString(4))
	trunc := smartmem.ReadString(b, e.p.At(0x1100), smartmem.CString(4).Truncate())
	w := smartmem.ReadString(b, e.p.At(0x1200), smartmem.UTF16(16))
	viaPtr := smartmem.ReadString(b, e.p.Chain(e.p.At(0x1300), 0), smartmem.CString(32))
	us := smartmem.ReadString(b, e.p.At(0x2000), smartmem.UnicodeString)
	long := smartmem.ReadString(b, e.p.At(0x2100), smartmem.UnicodeString)
	empty := smartmem.ReadString(b, e.p.At(0x2200), smartmem.UnicodeString)
	nullBuf := smartmem.ReadString(b, e.p.At(0x2300), smartmem.UnicodeString)
	e.run(b)

	for name, tc := range map[string]struct {
		f    *smartmem.Future[string]
		want string
	}{
		"CString": {c, "player"}, "Truncate": {trunc, "abcd"}, "UTF16": {w, "Ünïcode"},
		"via pointer": {viaPtr, "player"}, "UnicodeString": {us, "C:\\x"},
		"capped by WithMaxStringLen": {long, "too "}, "empty": {empty, ""},
	} {
		if got := get(t, tc.f); got != tc.want {
			t.Errorf("%s = %q, want %q", name, got, tc.want)
		}
	}
	if !errors.Is(getErr(t, noTerm), smartmem.ErrNoTerminator) {
		t.Error("missing terminator not reported")
	}
	if !errors.Is(getErr(t, nullBuf), smartmem.ErrNullBuffer) {
		t.Error("null buffer not reported")
	}
}

// A UNICODE_STRING takes two rounds: the header, then the characters.
func TestUnicodeStringRounds(t *testing.T) {
	e := newEnv(t)
	unicodeString(e, 0x2000, 0x3000, "hi")
	b := e.s.NewBatch()
	f := smartmem.ReadString(b, e.p.At(0x2000), smartmem.UnicodeString)
	e.run(b)
	if get(t, f) != "hi" {
		t.Fatal("wrong value")
	}
	e.assertTrace(`
		r1 pid=100 R 0x2000+16
		r2 pid=100 R 0x3000+4`)
	mustPanic(t, "zero kind", "zero StringKind", func() {
		smartmem.ReadString(e.s.NewBatch(), e.p.At(0), smartmem.StringKind{})
	})
}
