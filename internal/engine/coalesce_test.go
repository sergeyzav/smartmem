package engine

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// Z2: every read is covered by exactly one range; a range never crosses a
// page unless it is a single read that crosses one itself; ranges on the
// same page do not overlap.
func FuzzZ2_Coalesce(f *testing.F) {
	for seed := range uint64(8) {
		f.Add(seed, uint8(30), uint8(64))
	}
	f.Fuzz(func(t *testing.T, seed uint64, n, gap uint8) {
		rng := rand.New(rand.NewPCG(seed, ^seed))
		reads := make([]*Node, int(n)%64+1)
		for i := range reads {
			reads[i] = &Node{Kind: Leaf, Size: uint32(rng.IntN(48) + 1)}
			reads[i].addr = 0x10000 + rng.Uint64N(3*PageSize)
		}
		slices.SortFunc(reads, compareNodes)
		spans := coalesce(reads, uint64(gap), nil)

		covered := 0
		for i, s := range spans {
			if s.first >= s.last {
				t.Fatalf("span %d is empty", i)
			}
			covered += s.last - s.first
			if page(s.start) != page(s.end-1) && s.last-s.first != 1 {
				t.Fatalf("span %d [%#x,%#x) crosses a page with %d reads", i, s.start, s.end, s.last-s.first)
			}
			for _, r := range reads[s.first:s.last] {
				if r.addr < s.start || r.addr+uint64(r.Size) > s.end {
					t.Fatalf("read [%#x,+%d) outside span [%#x,%#x)", r.addr, r.Size, s.start, s.end)
				}
			}
			if i > 0 && spans[i-1].last != s.first {
				t.Fatalf("spans %d and %d are not consecutive", i-1, i)
			}
			if i > 0 && page(spans[i-1].start) == page(s.start) && spans[i-1].end > s.start &&
				page(spans[i-1].start) == page(spans[i-1].end-1) && page(s.start) == page(s.end-1) {
				t.Fatalf("spans %d and %d overlap on one page", i-1, i)
			}
		}
		if covered != len(reads) {
			t.Fatalf("%d of %d reads covered", covered, len(reads))
		}
	})
}
