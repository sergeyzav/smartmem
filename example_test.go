package smartmem_test

import (
	"context"
	"fmt"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

// Read describes requests in a Batch; Run executes them in as few rounds as
// their pointer chains allow.
func ExampleRead() {
	mem := fake.New()
	mem.Proc(4242).U32(0x1000, 100).U32(0x1004, 3)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	b := s.NewBatch()
	hp := smartmem.Read[int32](b, p.At(0x1000))
	team := smartmem.Read[uint8](b, p.At(0x1004))
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}

	v, _ := hp.Get()
	t, _ := team.Get()
	fmt.Println(v, t)
	// Output: 100 3
}

// Chain follows pointers with the Cheat Engine notation:
// Chain(B, 0x30, 0x90) is [[B]+0x30]+0x90.
func ExampleProcess_Chain() {
	mem := fake.New()
	mem.Proc(4242).
		Module("game.dll", 0x7ff600000000, 0x1000).
		U64(0x7ff600000100, 0x20000). // game.dll+0x100 -> object
		U64(0x20030, 0x30000).        // object+0x30 -> transform
		Bytes(0x30090, []byte{0, 0, 0x80, 0x3f, 0, 0, 0, 0x40, 0, 0, 0x40, 0x40})

	type Vec3 struct{ X, Y, Z float32 }

	s := smartmem.New(mem)
	p, _ := s.Process(4242)
	loc := p.Chain(p.Module("game.dll").Add(0x100), 0x30, 0x90)

	pos, err := smartmem.ReadNow[Vec3](context.Background(), loc)
	fmt.Println(loc, pos, err)
	// Output: [[game.dll+0x100]+0x30]+0x90 {1 2 3} <nil>
}
