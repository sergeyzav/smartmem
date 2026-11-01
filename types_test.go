package smartmem_test

import (
	"errors"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/internal/codec"
)

type node struct {
	Value int32
	Live  smartmem.CBool
	_     [3]byte
	Next  smartmem.Ptr[node]
	Base  smartmem.Addr
}

// C1, root types: Ptr, Addr and CBool are blittable with C sizes.
func TestRootTypesAreBlittable(t *testing.T) {
	if c := codec.MustOf[smartmem.Ptr[node]](); c.Size != 8 {
		t.Errorf("Ptr size %d, want 8", c.Size)
	}
	if c := codec.MustOf[smartmem.CBool](); c.Size != 1 {
		t.Errorf("CBool size %d, want 1", c.Size)
	}
	if c := codec.MustOf[node](); c.Size != 24 {
		t.Errorf("node size %d, want 24", c.Size)
	}

	_, err := codec.Of[struct{ B bool }]()
	var te *smartmem.TypeError
	if !errors.As(err, &te) {
		t.Errorf("bool field: err=%v, want *smartmem.TypeError", err)
	}
}

func TestPtrAndCBool(t *testing.T) {
	var nilPtr smartmem.Ptr[node]
	if !nilPtr.IsNil() || smartmem.Ptr[node](0x10).IsNil() {
		t.Error("IsNil is wrong")
	}
	p := newProc(t)
	if got := smartmem.Ptr[node](0x2000).Loc(p).String(); got != "0x2000" {
		t.Errorf("Ptr.Loc = %s", got)
	}
	if smartmem.CBool(0).Bool() || !smartmem.CBool(2).Bool() {
		t.Error("CBool.Bool is wrong")
	}
}
