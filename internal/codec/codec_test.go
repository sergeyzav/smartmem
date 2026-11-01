package codec_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/sergeyzav/smartmem/internal/codec"
)

type SomeStruct struct{ X, Y, Z float32 }

type (
	padded struct {
		A int8
		B int64
	}
	tail struct {
		A int32
		B int16
		C int8
	}
	nested struct {
		A int8
		V SomeStruct
		B int64
	}
	byteArr struct {
		A [3]uint8
		B uint32
	}
	unicode struct {
		Length, MaximumLength uint16
		_                     [4]byte
		Buffer                uint64
	}
	withBool struct{ Ok bool }
	deepBad  struct{ Inner struct{ Name string } }
	withInt  struct{ N int }
	withPtr  struct{ P *int32 }
	arrBad   struct{ A [2]bool }
	empty    struct{}
)

// C1: supported types are accepted; unsupported ones fail with the type and
// the path to the bad field.
func TestC1_TypeTable(t *testing.T) {
	ok := []reflect.Type{
		reflect.TypeFor[int8](), reflect.TypeFor[int16](), reflect.TypeFor[int32](), reflect.TypeFor[int64](),
		reflect.TypeFor[uint8](), reflect.TypeFor[uint16](), reflect.TypeFor[uint32](), reflect.TypeFor[uint64](),
		reflect.TypeFor[float32](), reflect.TypeFor[float64](),
		reflect.TypeFor[[4]uint16](), reflect.TypeFor[[2][3]int16](),
		reflect.TypeFor[SomeStruct](), reflect.TypeFor[padded](), reflect.TypeFor[nested](), reflect.TypeFor[unicode](),
	}
	for _, typ := range ok {
		c, err := codec.For(typ)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", typ, err)
			continue
		}
		if c.Kind != codec.Blittable || c.Size != typ.Size() {
			t.Errorf("%s: kind %d size %d, want blittable size %d", typ, c.Kind, c.Size, typ.Size())
		}
	}

	bad := []struct {
		typ  reflect.Type
		path string
		want string
	}{
		{reflect.TypeFor[bool](), "", "CBool"},
		{reflect.TypeFor[withBool](), "Ok", "CBool"},
		{reflect.TypeFor[arrBad](), "A[]", "CBool"},
		{reflect.TypeFor[string](), "", "string is not allowed"},
		{reflect.TypeFor[deepBad](), "Inner.Name", "string is not allowed"},
		{reflect.TypeFor[[]int32](), "", "slice is not allowed"},
		{reflect.TypeFor[withPtr](), "P", "ptr is not allowed"},
		{reflect.TypeFor[withInt](), "N", "platform-dependent size"},
		{reflect.TypeFor[uint](), "", "platform-dependent size"},
		{reflect.TypeFor[uintptr](), "", "Addr"},
		{reflect.TypeFor[map[int32]int32](), "", "map is not allowed"},
		{reflect.TypeFor[any](), "", "interface is not allowed"},
		{reflect.TypeFor[empty](), "", "zero-size"},
	}
	for _, tc := range bad {
		_, err := codec.For(tc.typ)
		var te *codec.TypeError
		if !errors.As(err, &te) {
			t.Errorf("%s: err=%v, want *TypeError", tc.typ, err)
			continue
		}
		if te.Type != tc.typ || te.Path != tc.path || !strings.Contains(te.Reason, tc.want) {
			t.Errorf("%s: got type=%s path=%q reason=%q; want path=%q reason containing %q",
				tc.typ, te.Type, te.Path, te.Reason, tc.path, tc.want)
		}
		if !strings.Contains(err.Error(), tc.typ.String()) {
			t.Errorf("%s: message %q does not name the type", tc.typ, err)
		}
	}
}

func TestMustOfPanicsWithTypeError(t *testing.T) {
	defer func() {
		r := recover()
		if _, ok := r.(*codec.TypeError); !ok {
			t.Fatalf("recovered %v (%T), want *TypeError", r, r)
		}
	}()
	codec.MustOf[withBool]()
}

// C2: Go layout matches the x64 C layout. Sizes and offsets are written out by
// hand from the C rules, not computed with unsafe, so a mismatch shows up.
func TestC2_LayoutMatchesC(t *testing.T) {
	cases := []struct {
		typ     reflect.Type
		size    uintptr
		offsets []uintptr // per field, in declaration order
	}{
		{reflect.TypeFor[padded](), 16, []uintptr{0, 8}},        // struct { int8_t a; int64_t b; }
		{reflect.TypeFor[tail](), 8, []uintptr{0, 4, 6}},        // struct { int32_t a; int16_t b; int8_t c; }
		{reflect.TypeFor[SomeStruct](), 12, []uintptr{0, 4, 8}}, // struct { float x, y, z; }
		{reflect.TypeFor[nested](), 24, []uintptr{0, 4, 16}},    // struct { int8_t a; struct { float x, y, z; } v; int64_t b; }
		{reflect.TypeFor[byteArr](), 8, []uintptr{0, 4}},        // struct { uint8_t a[3]; uint32_t b; }
		{reflect.TypeFor[unicode](), 16, []uintptr{0, 2, 4, 8}}, // UNICODE_STRING
		{reflect.TypeFor[[2][3]int16](), 12, nil},               // int16_t a[2][3]
	}
	for _, tc := range cases {
		c, err := codec.For(tc.typ)
		if err != nil {
			t.Fatalf("%s: %v", tc.typ, err)
		}
		if c.Size != tc.size {
			t.Errorf("%s: size %d, want %d", tc.typ, c.Size, tc.size)
		}
		for i, want := range tc.offsets {
			if got := tc.typ.Field(i).Offset; got != want {
				t.Errorf("%s: field %d offset %d, want %d", tc.typ, i, got, want)
			}
		}
	}
}

// C3: values decode from little-endian bytes, and decode/encode round-trips.
func TestC3_DecodePrimitives(t *testing.T) {
	check := func(name string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	check("int32", decode[int32](t, []byte{0xfe, 0xff, 0xff, 0xff}), int32(-2))
	check("uint16", decode[uint16](t, []byte{0x34, 0x12}), uint16(0x1234))
	check("uint64", decode[uint64](t, []byte{1, 2, 3, 4, 5, 6, 7, 8}), uint64(0x0807060504030201))
	check("float32", decode[float32](t, binary.LittleEndian.AppendUint32(nil, math.Float32bits(1.5))), float32(1.5))
	check("float64", decode[float64](t, binary.LittleEndian.AppendUint64(nil, math.Float64bits(-2.25))), float64(-2.25))
	check("SomeStruct", decode[SomeStruct](t, binary.LittleEndian.AppendUint32(
		binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, math.Float32bits(1)), math.Float32bits(2)),
		math.Float32bits(3))), SomeStruct{1, 2, 3})
	check("padded", decode[padded](t, []byte{7, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 9, 0, 0, 0, 0, 0, 0, 0}), padded{7, 9})
}

func FuzzC3_RoundTrip(f *testing.F) {
	f.Add(make([]byte, 24))
	f.Add(bytes.Repeat([]byte{0xff}, 24))
	f.Add([]byte("0123456789abcdefghijklmn"))
	c := codec.MustOf[nested]()
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) < int(c.Size) {
			return
		}
		var v nested
		c.Decode(unsafe.Pointer(&v), src)
		out := make([]byte, c.Size)
		c.Encode(out, unsafe.Pointer(&v))
		if !bytes.Equal(out, src[:c.Size]) {
			t.Fatalf("round trip changed bytes:\n in % x\nout % x", src[:c.Size], out)
		}
	})
}

func decode[T any](t *testing.T, src []byte) T {
	t.Helper()
	var v T
	codec.MustOf[T]().Decode(unsafe.Pointer(&v), src)
	return v
}

type sized struct{ A, B int32 }

func (sized) CSize() uintptr { return 8 }

type missized struct{ A int32 }

func (*missized) CSize() uintptr { return 8 }

// C5: a CSize method that disagrees with the Go size is an error.
func TestC5_CSize(t *testing.T) {
	if _, err := codec.Of[sized](); err != nil {
		t.Errorf("matching CSize: %v", err)
	}
	_, err := codec.Of[missized]()
	var te *codec.TypeError
	if !errors.As(err, &te) || !strings.Contains(te.Reason, "CSize() 8") {
		t.Errorf("mismatched CSize: err=%v", err)
	}
}

type record struct {
	ID     int32      `mem:"0x100"`
	Class  uint8      `mem:"0x13C"`
	Origin SomeStruct `mem:"48"`
	Cached int64      `mem:"-"`
	_      [8]byte
}

type (
	sparseUntagged struct {
		A int32 `mem:"0x10"`
		B int32
	}
	sparseBadTag struct {
		A int32 `mem:"oops"`
	}
	sparseBadField struct {
		A bool `mem:"0x10"`
	}
	sparseOnlySkips struct {
		A int32 `mem:"-"`
	}
	sparseNested struct{ E record }
)

// Sparse structs: tagged fields are read on their own; the Go layout does not matter.
func TestSparse(t *testing.T) {
	c, err := codec.Of[record]()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != codec.Sparse || len(c.Fields) != 3 {
		t.Fatalf("kind %d, %d fields; want sparse with 3 fields", c.Kind, len(c.Fields))
	}
	wantOffsets := map[string]uint64{"ID": 0x100, "Class": 0x13c, "Origin": 48}
	for _, f := range c.Fields {
		if f.MemOffset != wantOffsets[f.Name] {
			t.Errorf("%s: mem offset %#x, want %#x", f.Name, f.MemOffset, wantOffsets[f.Name])
		}
	}
	if got := c.MemSpan(); got != 0x13d {
		t.Errorf("MemSpan = %#x, want 0x13d", got)
	}

	var e record
	c.Fields[0].Decode(unsafe.Pointer(&e), []byte{100, 0, 0, 0})
	c.Fields[2].Decode(unsafe.Pointer(&e), make([]byte, 12))
	if e.ID != 100 {
		t.Errorf("ID = %d after field decode, want 100", e.ID)
	}

	bad := []struct {
		typ  reflect.Type
		path string
		want string
	}{
		{reflect.TypeFor[sparseUntagged](), "B", "needs mem"},
		{reflect.TypeFor[sparseBadTag](), "A", "bad mem tag"},
		{reflect.TypeFor[sparseBadField](), "A", "CBool"},
		{reflect.TypeFor[sparseOnlySkips](), "", "no fields"},
		{reflect.TypeFor[sparseNested](), "E", "Loader"},
	}
	for _, tc := range bad {
		_, err := codec.For(tc.typ)
		var te *codec.TypeError
		if !errors.As(err, &te) || te.Path != tc.path || !strings.Contains(te.Reason, tc.want) {
			t.Errorf("%s: err=%v; want path %q and reason containing %q", tc.typ, err, tc.path, tc.want)
		}
	}
}

func TestDecodeOnSparsePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Decode on a sparse type did not panic")
		}
	}()
	var e record
	codec.MustOf[record]().Decode(unsafe.Pointer(&e), make([]byte, 64))
}
