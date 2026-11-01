package smartmem_test

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

// fuzzQuery is one random request: a chain from base through offs, then a
// 4-byte or 16-byte read.
type fuzzQuery struct {
	base uint64
	offs []int64
	wide bool
}

// naiveResolve is the reference: it follows the chain one read at a time,
// straight from fake memory, without the engine.
func naiveResolve(p *fake.Proc, q fuzzQuery) ([]byte, error) {
	addr := q.base
	for k, off := range q.offs {
		b, err := p.Read(addr, 8)
		if err != nil {
			return nil, &smartmem.ReadError{Addr: addr, Err: err}
		}
		v := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
			uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
		switch {
		case v == 0:
			return nil, &smartmem.NullPointerError{Step: k}
		case v > 0x00007FFFFFFFFFFF:
			return nil, &smartmem.BadPointerError{Step: k, Value: v}
		}
		addr = v + uint64(off)
	}
	n := 4
	if q.wide {
		n = 16
	}
	b, err := p.Read(addr, n)
	if err != nil {
		return nil, &smartmem.ReadError{Addr: addr, Err: err}
	}
	return b, nil
}

// Z1: on random memory and random chains, the engine returns what the naive
// resolver returns, in max depth + 1 rounds, with one Exec per round.
func FuzzZ1_EngineVsNaive(f *testing.F) {
	for seed := range uint64(8) {
		f.Add(seed, uint8(20))
	}
	f.Fuzz(func(t *testing.T, seed uint64, nq uint8) {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		e := newEnv(t)

		// Objects of 0x100 bytes, some straddling a page boundary. Each
		// 8-byte slot holds a pointer to another object, null, a kernel
		// address, an unmapped address, or noise.
		const nobj = 24
		objs := make([]uint64, nobj)
		for i := range objs {
			objs[i] = 0x10000 + uint64(i)*0x180
		}
		for _, o := range objs {
			for slot := uint64(0); slot < 0x100; slot += 8 {
				var v uint64
				switch rng.IntN(10) {
				case 0:
					v = 0
				case 1:
					v = 0xfffff80000000000 + rng.Uint64N(0x1000)
				case 2:
					v = 0x7000000 + rng.Uint64N(0x1000)
				case 3:
					v = rng.Uint64()
				default:
					v = objs[rng.IntN(nobj)]
				}
				e.fp.U64(o+slot, v)
			}
		}
		e.fp.Unmap(objs[rng.IntN(nobj)])

		queries := make([]fuzzQuery, int(nq)%40+1)
		maxDepth := 0
		for i := range queries {
			q := fuzzQuery{base: objs[rng.IntN(nobj)] + 8*rng.Uint64N(32), wide: rng.IntN(2) == 0}
			for range rng.IntN(5) {
				q.offs = append(q.offs, int64(8*rng.IntN(32)))
			}
			maxDepth = max(maxDepth, len(q.offs))
			queries[i] = q
		}

		b := e.s.NewBatch()
		narrow := make([]*smartmem.Future[uint32], len(queries))
		wide := make([]*smartmem.Future[[16]byte], len(queries))
		for i, q := range queries {
			loc := e.p.Chain(e.p.At(q.base), q.offs...)
			if q.wide {
				wide[i] = smartmem.Read[[16]byte](b, loc)
			} else {
				narrow[i] = smartmem.Read[uint32](b, loc)
			}
		}
		e.run(b)

		for i, q := range queries {
			want, wantErr := naiveResolve(e.fp, q)
			var got []byte
			var err error
			if q.wide {
				var v [16]byte
				v, err = wide[i].Get()
				got = v[:]
			} else {
				var v uint32
				v, err = narrow[i].Get()
				got = []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
			}
			if !sameError(err, wantErr) {
				t.Fatalf("query %d %+v: err %v, naive %v", i, q, err, wantErr)
			}
			if err == nil && string(got) != string(want) {
				t.Fatalf("query %d %+v: got % x, naive % x", i, q, got, want)
			}
		}

		// Only the rounds a request reaches are executed, so the trace has at
		// most maxDepth+1 rounds, and one Exec (line) per round.
		lines := strings.Split(strings.TrimSpace(e.mem.Trace()), "\n")
		if len(lines) > maxDepth+1 {
			t.Fatalf("%d Execs for max depth %d:\n%s", len(lines), maxDepth, e.mem.Trace())
		}
		for i, l := range lines {
			if !strings.HasPrefix(l, "r"+string(rune('1'+i))+" ") {
				t.Fatalf("Exec %d is not round %d:\n%s", i, i+1, e.mem.Trace())
			}
		}
	})
}

func sameError(got, want error) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	var gn, wn *smartmem.NullPointerError
	if errors.As(want, &wn) {
		return errors.As(got, &gn) && gn.Step == wn.Step
	}
	var gb, wb *smartmem.BadPointerError
	if errors.As(want, &wb) {
		return errors.As(got, &gb) && *gb == *wb
	}
	var gr, wr *smartmem.ReadError
	if errors.As(want, &wr) {
		return errors.As(got, &gr) && gr.Addr == wr.Addr
	}
	return false
}
