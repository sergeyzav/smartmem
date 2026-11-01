package smartmem

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	"github.com/sergeyzav/smartmem/internal/codec"
	"github.com/sergeyzav/smartmem/internal/engine"
)

// ErrUnstable means a pointer of a loaded struct changed while it was read,
// so the struct may mix two objects, and the retries set by Revalidate ran out.
var ErrUnstable = errors.New("smartmem: pointer changed while reading")

// FieldError is the error of a struct whose field could not be read.
type FieldError struct {
	Loc Loc
	Err error
}

func (e *FieldError) Error() string { return fmt.Sprintf("smartmem: field at %v: %v", e.Loc, e.Err) }
func (e *FieldError) Unwrap() error { return e.Err }

// Binder is a Go struct that knows where its fields live in target memory.
// Bind is called once, when the struct is registered with Load or LoadEach:
// it describes the shape and must not depend on values read.
type Binder interface {
	Bind(l *Loader, base Loc)
}

// Loader collects the fields of one struct. Bind methods receive it.
type Loader struct {
	st *loadState
}

// LoadOption configures Load and LoadEach.
type LoadOption func(*loadOptions)

type loadOptions struct {
	alap       bool
	revalidate int
	strict     bool
}

// WithALAP turns "as late as possible" scheduling on or off; it is on by
// default. With it, every chain of a struct is delayed so that all fields are
// read in the last round, by one scatter operation.
func WithALAP(on bool) LoadOption { return func(o *loadOptions) { o.alap = on } }

// Revalidate re-reads every pointer of the struct's chains in the round its
// fields are read. If one changed, the object may have moved, and the struct
// is read again, up to retries times; then it fails with ErrUnstable.
func Revalidate(retries int) LoadOption {
	return func(o *loadOptions) { o.revalidate = max(0, retries) }
}

// Strict makes LoadEach fail as a whole when any element fails. By default
// failed elements are left out.
func Strict() LoadOption { return func(o *loadOptions) { o.strict = true } }

// FieldOption configures a field.
type FieldOption func(*fieldReq)

// Optional lets the struct load when this field fails; the field is then zero.
func Optional() FieldOption { return func(f *fieldReq) { f.optional = true } }

// Field describes a field read: dst points into the struct being bound, and
// T must be blittable.
func Field[T any](l *Loader, dst *T, loc Loc, opts ...FieldOption) {
	c := codec.MustOf[T]()
	if c.Kind != codec.Blittable {
		panic("smartmem: Field: " + c.Type.String() + " is a sparse struct; bind it with Sub")
	}
	l.st.addField(loc, uint32(c.Size), func(b []byte) error {
		c.Decode(unsafe.Pointer(dst), b)
		return nil
	}, func() { var zero T; *dst = zero }, opts)
}

// FieldString describes a string field.
func FieldString(l *Loader, dst *string, loc Loc, kind StringKind, opts ...FieldOption) {
	f := l.st.newField(loc, func() { *dst = "" }, opts)
	f.head = l.st.g.newChain(loc, newStringLeaf(l.st.g, kind, func(s string, err error) {
		if err == nil {
			*dst = s
		}
		f.st.fieldDone(f, err)
	})).head
	if kind.enc == encUnicodeString {
		f.depth++
	}
}

// Sub binds a nested struct: its fields join the outer struct and are
// published with it.
func Sub[T any, PT interface {
	*T
	Binder
}](l *Loader, dst *T, base Loc,
) {
	PT(dst).Bind(l, base)
}

// Load describes a read of a whole struct: Bind describes its fields, the
// engine reads them in as few rounds as their chains allow, and the Future
// gets the struct only when every field is in. A struct is never seen half
// filled. The first failing field (unless Optional) fails the struct, and its
// other fields are not read further.
func Load[T any, PT interface {
	*T
	Binder
}](s Scope, base Loc, opts ...LoadOption,
) *Future[T] {
	g := s.scope()
	g.checkLoc(base)
	f := newFuture[T](g)
	staging := new(T)
	st := newLoadState(g, opts, func(err error) {
		if err != nil {
			f.resolve(*new(T), err)
			return
		}
		f.sv = *staging
		f.resolveStaged(nil)
	})
	PT(staging).Bind(&Loader{st: st}, base)
	st.finish()
	g.add(st)
	return f
}

// readSparse reads a struct with mem tags: each tagged field is read from
// loc plus its offset, and the struct is published when all are in.
func readSparse[T any](g *graph, f *Future[T], c *codec.Codec, loc Loc) {
	staging := new(T)
	st := newLoadState(g, nil, func(err error) {
		if err != nil {
			f.resolve(*new(T), err)
			return
		}
		f.sv = *staging
		f.resolveStaged(nil)
	})
	for i := range c.Fields {
		fd := &c.Fields[i]
		st.addField(loc.Add(int64(fd.MemOffset)), uint32(fd.Codec.Size), func(b []byte) error {
			fd.Decode(unsafe.Pointer(staging), b)
			return nil
		}, nil, nil)
	}
	st.finish()
	g.add(st)
}

// LoadEach loads a struct for every non-nil pointer in ptrs, in process p.
// The result keeps the order of ptrs. Nil pointers are skipped; failed
// elements are left out unless Strict is set. Bind runs once per element
// slot, not once per execution.
func LoadEach[T any, PT interface {
	*T
	Binder
}](s Scope, p *Process, ptrs *Future[[]Ptr[T]], opts ...LoadOption,
) *Future[[]T] {
	g := s.scope()
	if p.s != g.s {
		panic("smartmem: LoadEach: the Process belongs to another Session")
	}
	e := &eachState[T]{g: g, out: newFuture[[]T](g), opts: opts}
	var o loadOptions
	for _, opt := range opts {
		opt(&o)
	}
	e.strict = o.strict
	e.bind = func(slot *eachSlot[T]) {
		base := Loc{proc: p, kind: baseCell, cell: &slot.addr, offs: []int64{0}}
		PT(slot.staging).Bind(&Loader{st: slot.st}, base)
	}
	ptrs.subscribe(e.start)
	return e.out
}

type eachState[T any] struct {
	g       *graph
	out     *Future[[]T]
	opts    []LoadOption
	strict  bool
	bind    func(*eachSlot[T])
	slots   []*eachSlot[T]
	active  []*eachSlot[T]
	pending int
}

type eachSlot[T any] struct {
	addr    uint64
	staging *T
	st      *loadState
	err     error
}

func (e *eachState[T]) start(ptrs []Ptr[T], err error) {
	if err != nil {
		e.out.resolve(nil, err)
		return
	}
	e.active = e.active[:0]
	for i, ptr := range ptrs {
		if ptr.IsNil() {
			continue
		}
		for len(e.slots) <= i {
			e.slots = append(e.slots, nil)
		}
		slot := e.slots[i]
		if slot == nil {
			slot = &eachSlot[T]{staging: new(T)}
			slot.st = newLoadState(e.g, e.opts, func(err error) { e.slotDone(slot, err) })
			e.bind(slot)
			slot.st.finish()
			e.slots[i] = slot
		}
		slot.addr, slot.err = ptr.Addr(), nil
		e.active = append(e.active, slot)
	}
	e.pending = len(e.active)
	if e.pending == 0 {
		e.out.resolve([]T{}, nil)
		return
	}
	for _, slot := range e.active {
		slot.st.schedule(e.g.run)
	}
}

func (e *eachState[T]) slotDone(slot *eachSlot[T], err error) {
	slot.err = err
	e.pending--
	if e.pending > 0 {
		return
	}
	out := make([]T, 0, len(e.active))
	for _, s := range e.active {
		if s.err != nil {
			if e.strict {
				e.out.resolve(nil, s.err)
				return
			}
			continue
		}
		out = append(out, *s.staging)
	}
	e.out.resolve(out, nil)
}

// loadState runs the fields of one struct and publishes it once.
type loadState struct {
	g       *graph
	opts    loadOptions
	fields  []*fieldReq
	depth   int // the deepest static chain, in rounds after the base is known
	publish func(error)

	pending  int
	done     bool
	skip     bool // set when the struct failed: its nodes are dropped
	r0       int  // the round the base became known
	retries  int
	unstable bool
	checks   []*checkNode
	nchecks  int
}

func newLoadState(g *graph, opts []LoadOption, publish func(error)) *loadState {
	st := &loadState{g: g, opts: loadOptions{alap: true}, publish: publish}
	for _, o := range opts {
		o(&st.opts)
	}
	return st
}

// fieldReq is one field: a chain that ends in a leaf whose Sink it is.
type fieldReq struct {
	st       *loadState
	loc      Loc
	head     *engine.Node
	depth    int
	optional bool
	decode   func([]byte) error
	zero     func()
}

func (st *loadState) newField(loc Loc, zero func(), opts []FieldOption) *fieldReq {
	st.g.checkLoc(loc)
	n, _ := loc.Depth()
	f := &fieldReq{st: st, loc: loc, depth: n, zero: zero}
	for _, o := range opts {
		o(f)
	}
	st.fields = append(st.fields, f)
	return f
}

func (st *loadState) addField(loc Loc, size uint32, decode func([]byte) error, zero func(), opts []FieldOption) {
	f := st.newField(loc, zero, opts)
	f.decode = decode
	f.head = st.g.newChain(loc, &engine.Node{Kind: engine.Leaf, Size: size, Sink: f}).head
}

// Done is the Sink of a value field's leaf.
func (f *fieldReq) Done(b []byte, err error) {
	if err == nil {
		err = f.decode(b)
	}
	f.st.fieldDone(f, err)
}

func (st *loadState) finish() {
	for _, f := range st.fields {
		st.depth = max(st.depth, f.depth)
	}
	for _, f := range st.fields {
		for n := f.head; n != nil; n = n.Next {
			n.Skip = &st.skip
			if n.Kind == engine.Deref && st.opts.revalidate > 0 {
				n.Observer = st
			}
		}
		if st.opts.alap {
			f.head.Delay = st.depth - f.depth
		}
	}
}

func (st *loadState) schedule(r *engine.Run) {
	st.retries = 0
	st.start(r)
}

// start schedules every field; a retry calls it again.
func (st *loadState) start(r *engine.Run) {
	st.pending, st.done, st.skip, st.unstable, st.nchecks = len(st.fields), false, false, false, 0
	st.r0 = r.Round() + 1
	if len(st.fields) == 0 {
		st.complete(nil)
		return
	}
	for _, f := range st.fields {
		base, err := f.loc.base()
		if err != nil {
			st.fieldDone(f, err)
			continue
		}
		r.Add(f.head, base+uint64(f.loc.offs[0]))
	}
}

func (st *loadState) fieldDone(f *fieldReq, err error) {
	if st.done {
		return
	}
	if err != nil {
		if !f.optional {
			st.skip = true
			st.complete(&FieldError{Loc: f.loc, Err: err})
			return
		}
		if f.zero != nil {
			f.zero()
		}
	}
	st.pending--
	st.maybeComplete()
}

func (st *loadState) maybeComplete() {
	if st.pending > 0 || st.done {
		return
	}
	if st.unstable {
		if st.retries < st.opts.revalidate {
			st.retries++
			st.start(st.g.run)
			return
		}
		st.complete(ErrUnstable)
		return
	}
	st.complete(nil)
}

func (st *loadState) complete(err error) {
	st.done = true
	st.publish(err)
}

// Deref implements engine.DerefObserver: every pointer a chain reads is
// re-read in the struct's last round, together with its fields.
func (st *loadState) Deref(n *engine.Node, slot, value uint64) {
	if st.done {
		return
	}
	if st.nchecks == len(st.checks) {
		c := &checkNode{st: st}
		c.node.Kind, c.node.Size, c.node.Sink = engine.Leaf, engine.PtrSize, c
		c.node.Skip = &st.skip
		st.checks = append(st.checks, c)
	}
	c := st.checks[st.nchecks]
	st.nchecks++
	c.want = value
	c.node.PID, c.node.Flags = n.PID, n.Flags
	st.pending++
	st.g.run.AddAt(&c.node, slot, st.r0+st.depth)
}

// checkNode re-reads one pointer for Revalidate.
type checkNode struct {
	st   *loadState
	node engine.Node
	want uint64
}

func (c *checkNode) Done(b []byte, err error) {
	st := c.st
	if st.done {
		return
	}
	if err != nil || binary.LittleEndian.Uint64(b) != c.want {
		st.unstable = true
	}
	st.pending--
	st.maybeComplete()
}
