// Package fake is an in-memory Backend for tests. It needs no memory dump and
// no native libraries, so code built on smartmem, including Bind methods, can
// be tested on any OS.
//
// Memory is kept in 4 KiB pages. A page that was never written or mapped is
// unreadable, the same as an unmapped page in a real process.
package fake

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/sergeyzav/smartmem/backend"
)

// PageSize is the page granularity of the fake memory.
const PageSize = 4096

var (
	// ErrUnmapped is the error of a range that touches an unmapped page.
	ErrUnmapped = errors.New("fake: unmapped page")
	// ErrNoProcess is returned for an unknown pid.
	ErrNoProcess = errors.New("fake: no such process")
	// ErrNoModule is returned by ModuleBase for an unknown module.
	ErrNoModule = errors.New("fake: no such module")
)

// PartialMode says how a read that touches an unmapped page behaves.
type PartialMode int

const (
	// FailWhole fails the whole range: N = 0, Err = ErrUnmapped.
	FailWhole PartialMode = iota
	// Truncate returns the readable prefix: N < len(Dst), Err = nil.
	// If not even the first byte is readable, it fails like FailWhole.
	Truncate
)

// Memory is a fake Backend with a set of processes. It is safe for concurrent use.
type Memory struct {
	mu      sync.Mutex
	procs   map[uint32]*Proc
	partial PartialMode
	onRound func(round int, m *Memory)
	round   int
	execs   int
	tracing bool
	trace   strings.Builder
	scratch []traceOp
}

var (
	_ backend.Backend     = (*Memory)(nil)
	_ backend.ProcessInfo = (*Memory)(nil)
	_ backend.RoundAware  = (*Memory)(nil)
)

// New returns empty fake memory with tracing on.
func New() *Memory {
	return &Memory{procs: make(map[uint32]*Proc), tracing: true}
}

// Proc returns the process with this pid, creating a native x64 process if
// there is none.
func (m *Memory) Proc(pid uint32) *Proc {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.procs[pid]
	if p == nil {
		p = &Proc{m: m, pid: pid, pages: make(map[uint64]*[PageSize]byte), modules: make(map[string]uint64)}
		m.procs[pid] = p
	}
	return p
}

// SetPartialRead sets how reads over unmapped pages behave. The default is FailWhole.
func (m *Memory) SetPartialRead(mode PartialMode) {
	m.mu.Lock()
	m.partial = mode
	m.mu.Unlock()
}

// OnRound registers fn to run before the first Exec of each round, so a test
// can change memory between rounds. Round numbers start at 1.
func (m *Memory) OnRound(fn func(round int, m *Memory)) {
	m.mu.Lock()
	m.onRound = fn
	m.mu.Unlock()
}

// SetTrace turns trace recording on or off. Tracing allocates, so allocation
// tests turn it off.
func (m *Memory) SetTrace(on bool) {
	m.mu.Lock()
	m.tracing = on
	m.mu.Unlock()
}

// Trace returns one line per Exec: the round, the pid, then the writes and the
// reads, each sorted by address:
//
//	r1 pid=100 W 0x2000+4 R 0x1000+8 R 0x3000+8
func (m *Memory) Trace() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.trace.String()
}

// ResetTrace clears the trace and the Exec counter.
func (m *Memory) ResetTrace() {
	m.mu.Lock()
	m.trace.Reset()
	m.execs = 0
	m.mu.Unlock()
}

// Execs returns the number of Exec calls since New or ResetTrace.
func (m *Memory) Execs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.execs
}

// BeginRound implements backend.RoundAware.
func (m *Memory) BeginRound(round int) {
	m.mu.Lock()
	m.round = round
	fn := m.onRound
	m.mu.Unlock()
	if fn != nil {
		fn(round, m)
	}
}

// Exec implements backend.Backend. Writes go first, then reads. A write that
// touches an unmapped page writes nothing.
func (m *Memory) Exec(ctx context.Context, pid uint32, _ uint64, writes []backend.WriteOp, reads []backend.ReadOp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.procs[pid]
	if p == nil {
		return fmt.Errorf("%w: pid %d", ErrNoProcess, pid)
	}
	m.execs++
	for i := range writes {
		writes[i].Err = p.write(writes[i].Addr, writes[i].Src)
	}
	for i := range reads {
		reads[i].N, reads[i].Err = p.read(reads[i].Addr, reads[i].Dst, m.partial)
	}
	if m.tracing {
		m.appendTrace(pid, writes, reads)
	}
	return nil
}

type traceOp struct {
	write bool
	addr  uint64
	size  int
}

func (m *Memory) appendTrace(pid uint32, writes []backend.WriteOp, reads []backend.ReadOp) {
	ops := m.scratch[:0]
	for _, w := range writes {
		ops = append(ops, traceOp{write: true, addr: w.Addr, size: len(w.Src)})
	}
	for _, r := range reads {
		ops = append(ops, traceOp{addr: r.Addr, size: len(r.Dst)})
	}
	slices.SortStableFunc(ops, func(a, b traceOp) int {
		if a.write != b.write {
			if a.write {
				return -1
			}
			return 1
		}
		switch {
		case a.addr < b.addr:
			return -1
		case a.addr > b.addr:
			return 1
		}
		return a.size - b.size
	})
	m.scratch = ops
	fmt.Fprintf(&m.trace, "r%d pid=%d", m.round, pid)
	for _, op := range ops {
		kind := "R"
		if op.write {
			kind = "W"
		}
		fmt.Fprintf(&m.trace, " %s 0x%x+%d", kind, op.addr, op.size)
	}
	m.trace.WriteByte('\n')
}

// Info implements backend.ProcessInfo.
func (m *Memory) Info(pid uint32) (backend.ProcInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.procs[pid]
	if p == nil {
		return backend.ProcInfo{}, fmt.Errorf("%w: pid %d", ErrNoProcess, pid)
	}
	return backend.ProcInfo{PID: pid, X64: true, Wow64: p.wow64}, nil
}

// PidByName implements backend.ProcessInfo. Names are case-insensitive.
func (m *Memory) PidByName(name string) (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for pid, p := range m.procs {
		if p.name != "" && strings.EqualFold(p.name, name) {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrNoProcess, name)
}

// ModuleBase implements backend.ProcessInfo. Names are case-insensitive.
func (m *Memory) ModuleBase(pid uint32, name string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.procs[pid]
	if p == nil {
		return 0, fmt.Errorf("%w: pid %d", ErrNoProcess, pid)
	}
	base, ok := p.modules[strings.ToLower(name)]
	if !ok {
		return 0, fmt.Errorf("%w: %q in pid %d", ErrNoModule, name, pid)
	}
	return base, nil
}

// Proc is one fake process. Setters return the Proc so calls can be chained.
type Proc struct {
	m       *Memory
	pid     uint32
	name    string
	wow64   bool
	pages   map[uint64]*[PageSize]byte
	modules map[string]uint64
}

// PID returns the process id.
func (p *Proc) PID() uint32 { return p.pid }

// Name sets the process name used by PidByName.
func (p *Proc) Name(name string) *Proc {
	p.m.mu.Lock()
	p.name = name
	p.m.mu.Unlock()
	return p
}

// Wow64 marks the process as a 32-bit process on a 64-bit system.
func (p *Proc) Wow64() *Proc {
	p.m.mu.Lock()
	p.wow64 = true
	p.m.mu.Unlock()
	return p
}

// Module registers a module and maps size bytes of zeroed memory at base.
func (p *Proc) Module(name string, base uint64, size int) *Proc {
	p.m.mu.Lock()
	p.modules[strings.ToLower(name)] = base
	p.m.mu.Unlock()
	return p.Map(base, size)
}

// Map maps the pages that cover [addr, addr+size). Existing pages keep their data.
func (p *Proc) Map(addr uint64, size int) *Proc {
	if size <= 0 {
		return p
	}
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	for a := addr &^ (PageSize - 1); a < addr+uint64(size); a += PageSize {
		if p.pages[a] == nil {
			p.pages[a] = new([PageSize]byte)
		}
	}
	return p
}

// Unmap removes the page that contains addr.
func (p *Proc) Unmap(addr uint64) *Proc {
	p.m.mu.Lock()
	delete(p.pages, addr&^(PageSize-1))
	p.m.mu.Unlock()
	return p
}

// Bytes writes b at addr, mapping pages as needed.
func (p *Proc) Bytes(addr uint64, b []byte) *Proc {
	p.Map(addr, len(b))
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	if err := p.write(addr, b); err != nil {
		panic(err)
	}
	return p
}

// U64 writes a little-endian uint64 at addr (for example, a pointer).
func (p *Proc) U64(addr, v uint64) *Proc { return Put(p, addr, v) }

// U32 writes a little-endian uint32 at addr.
func (p *Proc) U32(addr uint64, v uint32) *Proc { return Put(p, addr, v) }

// I32 writes a little-endian int32 at addr.
func (p *Proc) I32(addr uint64, v int32) *Proc { return Put(p, addr, v) }

// Put writes v in little-endian layout at addr. v must be a fixed-size value
// accepted by encoding/binary.
func Put[T any](p *Proc, addr uint64, v T) *Proc {
	b, err := binary.Append(nil, binary.LittleEndian, v)
	if err != nil {
		panic(fmt.Sprintf("fake.Put: %v", err))
	}
	return p.Bytes(addr, b)
}

// Read reads n bytes outside of any scatter operation, for example as the
// reference in tests. It fails if any byte is unmapped.
func (p *Proc) Read(addr uint64, n int) ([]byte, error) {
	b := make([]byte, n)
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	if _, err := p.read(addr, b, FailWhole); err != nil {
		return nil, err
	}
	return b, nil
}

// read copies memory into dst; the caller holds p.m.mu.
func (p *Proc) read(addr uint64, dst []byte, mode PartialMode) (int, error) {
	n := 0
	for n < len(dst) {
		a := addr + uint64(n)
		page := p.pages[a&^(PageSize-1)]
		if page == nil {
			break
		}
		off := int(a & (PageSize - 1))
		n += copy(dst[n:], page[off:])
	}
	if n == len(dst) {
		return n, nil
	}
	clear(dst[n:])
	if mode == FailWhole || n == 0 {
		clear(dst)
		return 0, fmt.Errorf("%w: 0x%x", ErrUnmapped, addr+uint64(n))
	}
	return n, nil
}

// write copies src into memory if every page is mapped; the caller holds p.m.mu.
func (p *Proc) write(addr uint64, src []byte) error {
	if len(src) == 0 {
		return nil
	}
	for a := addr &^ (PageSize - 1); a < addr+uint64(len(src)); a += PageSize {
		if p.pages[a] == nil {
			return fmt.Errorf("%w: 0x%x", ErrUnmapped, a)
		}
	}
	n := 0
	for n < len(src) {
		a := addr + uint64(n)
		off := int(a & (PageSize - 1))
		n += copy(p.pages[a&^(PageSize-1)][off:], src[n:])
	}
	return nil
}
