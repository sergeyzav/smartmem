package backend_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend"
)

// errOutside is the error of a range outside the memory the backend has.
var errOutside = errors.New("address outside the image")

// readerAtBackend reads the memory of one process from an io.ReaderAt whose
// offset 0 is the address base. A memory image loaded at base works, and so
// does a live Linux process: os.Open("/proc/<pid>/mem") with base 0.
type readerAtBackend struct {
	mem  io.ReaderAt
	base uint64
}

// Exec runs the writes, then the reads. Errors of single ranges go to their
// ops; only a cancelled ctx fails the whole operation.
func (b *readerAtBackend) Exec(ctx context.Context, _ uint32, _ uint64, writes []backend.WriteOp, reads []backend.ReadOp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range writes {
		w := &writes[i]
		wa, ok := b.mem.(io.WriterAt)
		if !ok {
			w.Err = errors.ErrUnsupported
			continue
		}
		if _, err := wa.WriteAt(w.Src, b.offset(w.Addr)); err != nil {
			w.Err = err
		}
	}
	for i := range reads {
		r := &reads[i]
		if r.Addr < b.base {
			r.Err = errOutside
			continue
		}
		n, err := b.mem.ReadAt(r.Dst, b.offset(r.Addr))
		r.N = n
		switch {
		case n > 0 || err == nil:
			// read in full, or a short read: smartmem fails the requests
			// past N with *PartialReadError
		case errors.Is(err, io.EOF):
			r.Err = errOutside
		default:
			r.Err = err // nothing read: the range fails
		}
	}
	return nil
}

func (b *readerAtBackend) offset(addr uint64) int64 { return int64(addr - b.base) }

// A custom Backend only implements Exec. Here it reads a memory image that
// starts at 0x10000, and smartmem follows a pointer inside it.
func Example_readerAt() {
	const base = 0x10000
	img := make([]byte, 0x100)
	binary.LittleEndian.PutUint64(img[0x08:], 0x10040) // 0x10008: a pointer to 0x10040
	binary.LittleEndian.PutUint16(img[0x40:], 42)      // 0x10040: uint16 42
	copy(img[0x50:], "hello\x00")                      // 0x10050: a C string

	be := &readerAtBackend{mem: bytes.NewReader(img), base: base}
	s := smartmem.New(be)
	p, _ := s.Process(1) // without ProcessInfo, any pid is a native x64 process

	b := s.NewBatch()
	v := smartmem.Read[uint16](b, p.Chain(p.At(0x10008), 0)) // [0x10008]+0
	str := smartmem.ReadString(b, p.At(0x10050), smartmem.CString(16))
	out := smartmem.Read[uint64](b, p.At(0x20000)) // outside the image
	if err := b.Run(context.Background()); err != nil {
		panic(err)
	}
	fmt.Println(v.Get())
	fmt.Println(str.Get())
	_, err := out.Get()
	fmt.Println(err)
	// Output:
	// 42 <nil>
	// hello <nil>
	// smartmem: read 0x20000: address outside the image
}

// imageProcess adds the optional ProcessInfo interface: process and module
// lookups, so Session.ProcessByName, Process.Module and Process.PEB work.
type imageProcess struct {
	readerAtBackend
	pid     uint32
	name    string
	peb     uint64
	modules map[string]uint64 // lower-case name -> base
}

func (ip *imageProcess) Info(pid uint32) (backend.ProcInfo, error) {
	if pid != ip.pid {
		return backend.ProcInfo{}, fmt.Errorf("no process %d", pid)
	}
	return backend.ProcInfo{PID: pid, X64: true, PEB: ip.peb}, nil
}

func (ip *imageProcess) PidByName(name string) (uint32, error) {
	if !strings.EqualFold(name, ip.name) {
		return 0, fmt.Errorf("no process %q", name)
	}
	return ip.pid, nil
}

func (ip *imageProcess) ModuleBase(pid uint32, name string) (uint64, error) {
	base, ok := ip.modules[strings.ToLower(name)]
	if !ok || pid != ip.pid {
		return 0, fmt.Errorf("no module %q", name)
	}
	return base, nil
}

// With ProcessInfo, processes are found by name and locations can start at a
// module base. smartmem calls ModuleBase once per module and caches it.
func Example_processInfo() {
	img := make([]byte, 0x2000)
	binary.LittleEndian.PutUint32(img[0x1010:], 7) // core.dll+0x10

	be := &imageProcess{
		readerAtBackend: readerAtBackend{mem: bytes.NewReader(img), base: 0x10000},
		pid:             4242,
		name:            "app.exe",
		peb:             0x10000,
		modules:         map[string]uint64{"core.dll": 0x11000},
	}
	s := smartmem.New(be)
	p, err := s.ProcessByName("APP.EXE")
	if err != nil {
		panic(err)
	}
	v, err := smartmem.ReadNow[uint32](context.Background(), p.Module("Core.dll").Add(0x10))
	peb, _ := p.PEB()
	fmt.Println(p.PID(), v, err, peb)
	// Output: 4242 7 <nil> 0x10000
}
