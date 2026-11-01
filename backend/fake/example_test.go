package fake_test

import (
	"context"
	"fmt"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

// Record is the struct under test; Bind says where its fields live.
type Record struct {
	ID   int32
	Name string
}

func (r *Record) Bind(l *smartmem.Loader, base smartmem.Loc) {
	smartmem.Field(l, &r.ID, base.Add(0x10))
	smartmem.FieldString(l, &r.Name, base.Add(0x18).Deref(), smartmem.CString(16))
}

// A test lays out a process in fake memory, loads the struct through
// smartmem and checks both the value and the reads in each round.
func Example() {
	mem := fake.New()
	mem.Proc(100).
		I32(0x10010, 7).
		U64(0x10018, 0x20000).Bytes(0x20000, []byte("alpha\x00"))

	s := smartmem.New(mem)
	p, _ := s.Process(100)

	b := s.NewBatch()
	rec := smartmem.Load[Record](b, p.At(0x10000))
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	r, err := rec.Get()
	fmt.Printf("%+v %v\n", r, err)
	fmt.Print(mem.Trace())
	// Output:
	// {ID:7 Name:alpha} <nil>
	// r1 pid=100 R 0x10018+8
	// r2 pid=100 R 0x10010+4 R 0x20000+16
}

// OnRound changes memory between rounds, for example to test how code
// handles an object that moves while it is read.
func ExampleMemory_OnRound() {
	mem := fake.New()
	proc := mem.Proc(100).U64(0x1000, 0x2000).U32(0x2008, 1).U32(0x3008, 2)
	mem.OnRound(func(round int, _ *fake.Memory) {
		if round == 2 {
			proc.U32(0x2008, 99) // changed after the pointer was read
		}
	})

	s := smartmem.New(mem)
	p, _ := s.Process(100)
	v, err := smartmem.ReadNow[uint32](context.Background(), p.Chain(p.At(0x1000), 8))
	fmt.Println(v, err)
	// Output: 99 <nil>
}
