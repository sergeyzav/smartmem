package smartmem_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

func newProc(t *testing.T) *smartmem.Process {
	t.Helper()
	m := fake.New()
	m.Proc(100)
	p, err := smartmem.New(m).Process(100)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// L1: Chain is sugar for Deref/Add and builds exactly the same Loc.
// (The trace-level check comes with the engine, in stage 3.)
func TestL1_ChainEqualsDerefAdd(t *testing.T) {
	p := newProc(t)
	b := p.At(0x1000)
	cases := []struct {
		chain, explicit smartmem.Loc
		str             string
	}{
		{p.Chain(b), b, "0x1000"},
		{p.Chain(b, 0x30), b.Deref().Add(0x30), "[0x1000]+0x30"},
		{p.Chain(b, 0x30, 0x90), b.Deref().Add(0x30).Deref().Add(0x90), "[[0x1000]+0x30]+0x90"},
		{p.Chain(b.Add(0x10), 0, -8), b.Add(0x10).Deref().Deref().Add(-8), "[[0x1000+0x10]]-0x8"},
	}
	for _, tc := range cases {
		if !reflect.DeepEqual(tc.chain, tc.explicit) {
			t.Errorf("%s: Chain %v != explicit %v", tc.str, smartmem.LocOffsets(tc.chain), smartmem.LocOffsets(tc.explicit))
		}
		if got := tc.chain.String(); got != tc.str {
			t.Errorf("String() = %q, want %q", got, tc.str)
		}
	}
	if n, static := p.Chain(b, 1, 2, 3).Depth(); n != 3 || !static {
		t.Errorf("Depth = %d, %v; want 3, true", n, static)
	}
}

// L5: Locs are immutable: deriving from a shared prefix never changes the
// prefix or a sibling (the classic shared-backing-array bug with append).
func TestL5_Immutable(t *testing.T) {
	p := newProc(t)
	a := p.At(0x1000).Deref().Add(1)
	before := slices.Clone(smartmem.LocOffsets(a))

	x := a.Deref()
	y := a.Add(2)
	z := x.Add(5)
	w := x.Add(7)
	_ = a.Index(3, 16)

	check := func(name string, l smartmem.Loc, want []int64) {
		t.Helper()
		if got := smartmem.LocOffsets(l); !slices.Equal(got, want) {
			t.Errorf("%s: offsets %v, want %v", name, got, want)
		}
	}
	check("a", a, before)
	check("x", x, []int64{0, 1, 0})
	check("y", y, []int64{0, 3})
	check("z", z, []int64{0, 1, 5})
	check("w", w, []int64{0, 1, 7})
}

func TestLocString(t *testing.T) {
	p := newProc(t)
	cases := map[string]smartmem.Loc{
		"client.dll+0x1a2b30":           p.Module("client.dll").Add(0x1A2B30),
		"[[client.dll+0x10]+0x30]+0x90": p.Chain(p.Module("client.dll").Add(0x10), 0x30, 0x90),
		"0x1000-0x10":                   p.At(0x1000).Add(-0x10),
		"0x1000+0x80":                   p.At(0x1000).Index(2, 0x40),
		"<zero Loc>":                    {},
	}
	for want, l := range cases {
		if got := l.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestZeroLocPanics(t *testing.T) {
	var zero smartmem.Loc
	if !zero.IsZero() {
		t.Fatal("zero Loc is not IsZero")
	}
	for name, fn := range map[string]func(){
		"Add":   func() { zero.Add(1) },
		"Deref": func() { zero.Deref() },
		"Depth": func() { zero.Depth() },
		"Chain": func() { newProc(t).Chain(zero, 1) },
	} {
		mustPanic(t, name, "zero Loc", fn)
	}
}

func TestChainFromOtherProcessPanics(t *testing.T) {
	p1, p2 := newProc(t), newProc(t)
	mustPanic(t, "Chain", "another process", func() { p1.Chain(p2.At(0x1000), 8) })
}

func mustPanic(t *testing.T, name, want string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if s, _ := r.(string); !strings.Contains(s, want) {
			t.Errorf("%s: recovered %v, want a panic mentioning %q", name, r, want)
		}
	}()
	fn()
}
