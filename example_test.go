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

// Monster is a business struct assembled from several places in memory.
type Monster struct {
	HP   int32
	Name string
	Pos  struct{ X, Y, Z float32 }
}

// Bind says where each field lives. It runs once, when the struct is
// registered.
func (m *Monster) Bind(l *smartmem.Loader, base smartmem.Loc) {
	smartmem.Field(l, &m.HP, base.Add(0x10))
	smartmem.FieldString(l, &m.Name, base.Add(0x18).Deref(), smartmem.CString(16))
	smartmem.Field(l, &m.Pos, base.Add(0x20).Deref().Add(0x40))
}

// Load reads a struct whose fields are spread over memory. The engine reads
// them in as few rounds as the pointers allow, and the Future gets the struct
// only when every field is in.
func ExampleLoad() {
	mem := fake.New()
	mem.Proc(4242).
		I32(0x10010, 250).
		U64(0x10018, 0x20000).Bytes(0x20000, []byte("Grue\x00")).
		U64(0x10020, 0x30000).Bytes(0x30040, []byte{0, 0, 0x80, 0x3f, 0, 0, 0, 0x40, 0, 0, 0x40, 0x40})

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	b := s.NewBatch()
	monster := smartmem.Load[Monster](b, p.At(0x10000))
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	m, err := monster.Get()
	fmt.Printf("%+v %v\n", m, err)
	// Output: {HP:250 Name:Grue Pos:{X:1 Y:2 Z:3}} <nil>
}

// A Plan is described once and executed every tick, without allocating.
// View reads several values of the same tick.
func ExamplePlan() {
	mem := fake.New()
	game := mem.Proc(4242).U32(0x1000, 100).U32(0x1004, 50)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	plan := s.NewPlan()
	hp := smartmem.Read[int32](plan, p.At(0x1000))
	armor := smartmem.Read[int32](plan, p.At(0x1004))

	for tick := range 3 {
		game.I32(0x1000, int32(100-10*tick)) // the game changes memory
		if err := plan.Exec(context.Background()); err != nil {
			panic(err)
		}
		plan.View(func() {
			h, _ := hp.Get()
			a, _ := armor.Get()
			fmt.Println(plan.Gen(), h, a)
		})
	}
	// Output:
	// 1 100 50
	// 2 90 50
	// 3 80 50
}
