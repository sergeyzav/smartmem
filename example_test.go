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
	count := smartmem.Read[int32](b, p.At(0x1000))
	class := smartmem.Read[uint8](b, p.At(0x1004))
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}

	n, _ := count.Get()
	c, _ := class.Get()
	fmt.Println(n, c)
	// Output: 100 3
}

// Chain follows pointers in bracket notation:
// Chain(B, 0x30, 0x90) is [[B]+0x30]+0x90.
func ExampleProcess_Chain() {
	mem := fake.New()
	mem.Proc(4242).
		Module("core.dll", 0x7ff600000000, 0x1000).
		U64(0x7ff600000100, 0x20000). // core.dll+0x100 -> object
		U64(0x20030, 0x30000).        // object+0x30 -> geometry
		Bytes(0x30090, []byte{0, 0, 0x80, 0x3f, 0, 0, 0, 0x40, 0, 0, 0x40, 0x40})

	type Vec3 struct{ X, Y, Z float32 }

	s := smartmem.New(mem)
	p, _ := s.Process(4242)
	loc := p.Chain(p.Module("core.dll").Add(0x100), 0x30, 0x90)

	origin, err := smartmem.ReadNow[Vec3](context.Background(), loc)
	fmt.Println(loc, origin, err)
	// Output: [[core.dll+0x100]+0x30]+0x90 {1 2 3} <nil>
}

// Item is a business struct assembled from several places in memory.
type Item struct {
	ID     int32
	Name   string
	Origin struct{ X, Y, Z float32 }
}

// Bind says where each field lives. It runs once, when the struct is
// registered.
func (it *Item) Bind(l *smartmem.Loader, base smartmem.Loc) {
	smartmem.Field(l, &it.ID, base.Add(0x10))
	smartmem.FieldString(l, &it.Name, base.Add(0x18).Deref(), smartmem.CString(16))
	smartmem.Field(l, &it.Origin, base.Add(0x20).Deref().Add(0x40))
}

// Load reads a struct whose fields are spread over memory. The engine reads
// them in as few rounds as the pointers allow, and the Future gets the struct
// only when every field is in.
func ExampleLoad() {
	mem := fake.New()
	mem.Proc(4242).
		I32(0x10010, 250).
		U64(0x10018, 0x20000).Bytes(0x20000, []byte("alpha\x00")).
		U64(0x10020, 0x30000).Bytes(0x30040, []byte{0, 0, 0x80, 0x3f, 0, 0, 0, 0x40, 0, 0, 0x40, 0x40})

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	b := s.NewBatch()
	item := smartmem.Load[Item](b, p.At(0x10000))
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	it, err := item.Get()
	fmt.Printf("%+v %v\n", it, err)
	// Output: {ID:250 Name:alpha Origin:{X:1 Y:2 Z:3}} <nil>
}

// A Plan is described once and executed every tick, without allocating.
// View reads several values of the same tick.
func ExamplePlan() {
	mem := fake.New()
	target := mem.Proc(4242).U32(0x1000, 100).U32(0x1004, 50)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	plan := s.NewPlan()
	count := smartmem.Read[int32](plan, p.At(0x1000))
	limit := smartmem.Read[int32](plan, p.At(0x1004))

	for tick := range 3 {
		target.I32(0x1000, int32(100-10*tick)) // the target process changes memory
		if err := plan.Exec(context.Background()); err != nil {
			panic(err)
		}
		plan.View(func() {
			n, _ := count.Get()
			l, _ := limit.Get()
			fmt.Println(plan.Gen(), n, l)
		})
	}
	// Output:
	// 1 100 50
	// 2 90 50
	// 3 80 50
}

// A Scheduler merges requests from independent goroutines into shared rounds:
// here one subsystem reads a counter while another follows a pointer, and
// both are served by the same two scatter operations.
func ExampleScheduler() {
	mem := fake.New()
	mem.Proc(4242).U32(0x1000, 7).U64(0x2000, 0x5000).U32(0x5010, 42)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)
	sch := smartmem.NewScheduler(s)
	defer sch.Close()

	ctx := context.Background()
	results := make(chan string, 2)
	go func() { // subsystem A
		v, _ := smartmem.Read[uint32](sch, p.At(0x1000)).Wait(ctx)
		results <- fmt.Sprint("counter ", v)
	}()
	go func() { // subsystem B
		v, _ := smartmem.Read[uint32](sch, p.Chain(p.At(0x2000), 0x10)).Wait(ctx)
		results <- fmt.Sprint("chained ", v)
	}()
	a, b := <-results, <-results
	if a > b {
		a, b = b, a
	}
	fmt.Println(a)
	fmt.Println(b)
	// Output:
	// chained 42
	// counter 7
}
