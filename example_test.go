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

	type SomeStruct struct{ X, Y, Z float32 }

	s := smartmem.New(mem)
	p, _ := s.Process(4242)
	loc := p.Chain(p.Module("core.dll").Add(0x100), 0x30, 0x90)

	origin, err := smartmem.ReadNow[SomeStruct](context.Background(), loc)
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

// Requests in one Batch share rounds: the two plain reads and the first
// pointer of the chain go into round 1, and the trace of the fake backend
// shows one scatter operation per round.
func Example() {
	mem := fake.New()
	mem.Proc(4242).
		U32(0x1000, 7).U32(0x1008, 9).
		U64(0x2000, 0x5000).U32(0x5010, 42)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	b := s.NewBatch()
	a := smartmem.Read[uint32](b, p.At(0x1000))
	c := smartmem.Read[uint32](b, p.At(0x1008))
	d := smartmem.Read[uint32](b, p.Chain(p.At(0x2000), 0x10)) // [0x2000]+0x10
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}

	va, _ := a.Get()
	vc, _ := c.Get()
	vd, _ := d.Get()
	fmt.Println(va, vc, vd)
	fmt.Print(mem.Trace())
	// Output:
	// 7 9 42
	// r1 pid=4242 R 0x1000+12 R 0x2000+8
	// r2 pid=4242 R 0x5010+4
}

// Then follows a linked list: each element's address is known only after the
// previous one is read, so the walk takes one round per element. Resolved
// ends the walk with the collected values.
func ExampleThen() {
	// A list of three nodes: {next uint64; value uint32}.
	mem := fake.New()
	mem.Proc(4242).
		U64(0x1000, 0x2000).U32(0x1008, 10).
		U64(0x2000, 0x3000).U32(0x2008, 20).
		U64(0x3000, 0).U32(0x3008, 30)

	type node struct {
		Next  uint64
		Value uint32
		_     [4]byte
	}

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	var walk func(s smartmem.Scope, addr uint64, acc []uint32) *smartmem.Future[[]uint32]
	walk = func(s smartmem.Scope, addr uint64, acc []uint32) *smartmem.Future[[]uint32] {
		n := smartmem.Read[node](s, p.At(addr))
		return smartmem.Then(n, func(s smartmem.Scope, n node) *smartmem.Future[[]uint32] {
			acc = append(acc, n.Value)
			if n.Next == 0 {
				return smartmem.Resolved(s, acc)
			}
			return walk(s, n.Next, acc)
		})
	}

	b := s.NewBatch()
	values := walk(b, 0x1000, nil)
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	v, err := values.Get()
	fmt.Println(v, err, mem.Execs(), "rounds")
	// Output: [10 20 30] <nil> 3 rounds
}

// LoadEach loads a struct for every pointer of an array. Nil pointers are
// skipped, and all elements are read by the same rounds.
func ExampleLoadEach() {
	mem := fake.New()
	proc := mem.Proc(4242)
	for i, name := range []string{"alpha", "beta"} {
		base := uint64(0x10000 * (i + 1))
		proc.I32(base+0x10, int32(i+1)).
			U64(base+0x18, base+0x100).Bytes(base+0x100, append([]byte(name), 0)).
			U64(base+0x20, base+0x200)
	}
	// An array of three pointers; the middle one is nil.
	proc.U64(0x1000, 0x10000).U64(0x1008, 0).U64(0x1010, 0x20000)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	b := s.NewBatch()
	ptrs := smartmem.ReadSlice[smartmem.Ptr[Item]](b, p.At(0x1000), 3)
	items := smartmem.LoadEach[Item](b, p, ptrs)
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	list, _ := items.Get()
	for _, it := range list {
		fmt.Println(it.ID, it.Name)
	}
	// Output:
	// 1 alpha
	// 2 beta
}

// ReadString reads C strings, UTF-16 strings and Windows UNICODE_STRINGs.
func ExampleReadString() {
	mem := fake.New()
	mem.Proc(4242).
		Bytes(0x1000, []byte("core.dll\x00")).
		Bytes(0x2000, []byte{'h', 0, 'i', 0, 0, 0}).
		// UNICODE_STRING{Length: 4, MaximumLength: 6, Buffer: 0x2000}
		Bytes(0x3000, []byte{4, 0, 6, 0, 0, 0, 0, 0}).U64(0x3008, 0x2000)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	b := s.NewBatch()
	c := smartmem.ReadString(b, p.At(0x1000), smartmem.CString(32))
	w := smartmem.ReadString(b, p.At(0x2000), smartmem.UTF16(8))
	u := smartmem.ReadString(b, p.At(0x3000), smartmem.UnicodeString)
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	vc, _ := c.Get()
	vw, _ := w.Get()
	vu, _ := u.Get()
	fmt.Println(vc, vw, vu)
	// Output: core.dll hi hi
}

// Write runs before the reads of its round, so a read of the same address in
// the same Batch sees the new value.
func ExampleWrite() {
	mem := fake.New()
	mem.Proc(4242).U32(0x1000, 1)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	b := s.NewBatch()
	w := smartmem.Write(b, p.At(0x1000), uint32(5))
	r := smartmem.Read[uint32](b, p.At(0x1000))
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	_, werr := w.Get()
	v, _ := r.Get()
	fmt.Println(v, werr)
	// Output: 5 <nil>
}

// Run executes the Batches of two subsystems together, so their reads share
// one scatter operation.
func ExampleRun() {
	mem := fake.New()
	mem.Proc(4242).U32(0x1000, 1).U32(0x8000, 2)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)

	first, second := s.NewBatch(), s.NewBatch()
	a := smartmem.Read[uint32](first, p.At(0x1000))
	b := smartmem.Read[uint32](second, p.At(0x8000))
	if err := smartmem.Run(context.Background(), first, second); err != nil {
		panic(err)
	}
	va, _ := a.Get()
	vb, _ := b.Get()
	fmt.Println(va, vb, mem.Execs(), "scatter operation")
	// Output: 1 2 1 scatter operation
}

// Next follows the values of a Plan: a reader keeps the generation it saw and
// asks for a newer one.
func ExampleFuture_Next() {
	mem := fake.New()
	proc := mem.Proc(4242).U32(0x1000, 1)

	s := smartmem.New(mem)
	p, _ := s.Process(4242)
	plan := s.NewPlan()
	f := smartmem.Read[uint32](plan, p.At(0x1000))

	ctx := context.Background()
	var gen uint64
	for _, v := range []uint32{1, 2, 3} {
		proc.U32(0x1000, v)
		if err := plan.Exec(ctx); err != nil {
			panic(err)
		}
		var got uint32
		got, gen, _ = f.Next(ctx, gen)
		fmt.Println(gen, got)
	}
	// Output:
	// 1 1
	// 2 2
	// 3 3
}
