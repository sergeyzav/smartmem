package main

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

const modBase = 0x7ff600000000

func setup(t *testing.T) (*fake.Proc, *smartmem.Session, *smartmem.Process) {
	t.Helper()
	mem := fake.New()
	p := mem.Proc(100).Module("core.dll", modBase, 0x10000)
	p.U64(modBase+0x100, 0x20000).
		U64(0x20030, 0x30000).
		I32(0x30090, 100).
		U32(0x30040, math.Float32bits(1.5)).
		Bytes(0x40000, []byte("neo\x00"))
	s := smartmem.New(mem)
	proc, err := s.Process(100)
	if err != nil {
		t.Fatal(err)
	}
	return p, s, proc
}

func TestParse(t *testing.T) {
	_, _, p := setup(t)
	for spec, want := range map[string]string{
		"count=core.dll+0x100,0x30,0x90:i32": "[[core.dll+0x100]+0x30]+0x90",
		"x=core.dll+0x100:ptr":               "core.dll+0x100",
		"n=0x40000:str":                      "0x40000",
		"neg=0x40000,-0x10:u8":               "[0x40000]-0x10",
	} {
		_, loc, _, err := parse(p, spec)
		if err != nil || loc.String() != want {
			t.Errorf("%s: got %s, %v; want %s", spec, loc, err, want)
		}
	}
	for _, bad := range []string{"count", "count=core.dll+0x100", "count=core.dll+zz:i32", "count=0x10,0xq:i32"} {
		if _, _, _, err := parse(p, bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestPollPrintsChanges(t *testing.T) {
	mem, s, p := setup(t)
	w, err := newWatcher(s, p, []string{
		"count=core.dll+0x100,0x30,0x90:i32",
		"x=core.dll+0x100,0x30,0x40:f32",
		"name=0x40000:str",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	poll := func(want ...string) {
		t.Helper()
		got, err := w.poll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	}

	poll(`count = 100`, `x = 1.5`, `name = "neo"`)
	poll()
	mem.I32(0x30090, 90)
	poll(`count = 90`)
	mem.U64(0x20030, 0)
	poll(`count = error: smartmem: null pointer at deref 1`, `x = error: smartmem: null pointer at deref 1`)
	mem.U64(0x20030, 0x30000)
	poll(`count = 90`, `x = 1.5`)
}

func TestUnknownType(t *testing.T) {
	_, s, p := setup(t)
	if _, err := newWatcher(s, p, []string{"count=0x1000:i128"}); err == nil {
		t.Fatal("no error")
	}
}
