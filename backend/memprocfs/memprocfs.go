// Package memprocfs is the smartmem Backend over MemProcFS, through
// gomemprocfs. It needs the MemProcFS native libraries at run time; see the
// gomemprocfs README.
//
//	vmm, err := gm.NewVmm(libPath, gm.WithDevice("fpga"))
//	...
//	be := memprocfs.New(vmm)
//	defer be.Close()
//	s := smartmem.New(be)
package memprocfs

import (
	"context"
	"fmt"
	"sync"

	gm "github.com/sergeyzav/gomemprocfs"

	"github.com/sergeyzav/smartmem/backend"
)

// Backend runs smartmem scatter operations on a gomemprocfs Vmm. It keeps a
// pool of scatter handles and is safe for concurrent use.
type Backend struct {
	vmm *gm.Vmm

	mu     sync.Mutex
	pool   []*gm.ScatterHandle
	closed bool
}

var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.ProcessInfo = (*Backend)(nil)
)

// New returns a Backend over vmm. The caller keeps ownership of vmm and
// closes it after Backend.Close.
func New(vmm *gm.Vmm) *Backend { return &Backend{vmm: vmm} }

// Close releases the pooled scatter handles.
func (b *Backend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, h := range b.pool {
		h.Close()
	}
	b.pool, b.closed = nil, true
}

// handle returns a scatter handle for pid and flags: a pooled one retargeted
// with Clear, or a new one.
func (b *Backend) handle(pid uint32, flags gm.MemFlag) (*gm.ScatterHandle, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, gm.ErrClosed
	}
	var h *gm.ScatterHandle
	if n := len(b.pool); n > 0 {
		h = b.pool[n-1]
		b.pool = b.pool[:n-1]
	}
	b.mu.Unlock()
	if h != nil {
		if err := h.Clear(pid, flags); err == nil {
			return h, nil
		}
		h.Close()
	}
	return b.vmm.ScatterInitialize(pid, flags)
}

func (b *Backend) release(h *gm.ScatterHandle) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		h.Close()
		return
	}
	b.pool = append(b.pool, h)
}

// Exec implements backend.Backend. MemProcFS does not report the result of
// single writes, so a write fails only if it could not be prepared. A read
// that returns no bytes is reported as gomemprocfs.ErrReadFailed; a short
// read keeps the bytes that were read.
func (b *Backend) Exec(ctx context.Context, pid uint32, flags uint64, writes []backend.WriteOp, reads []backend.ReadOp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(writes) == 0 && len(reads) == 0 {
		return nil
	}
	h, err := b.handle(pid, gm.MemFlag(flags))
	if err != nil {
		return err
	}
	for i := range writes {
		writes[i].Err = h.PrepareWrite(writes[i].Addr, writes[i].Src)
	}
	for i := range reads {
		reads[i].N, reads[i].Err = 0, h.Prepare(reads[i].Addr, uint32(len(reads[i].Dst)))
	}
	if len(writes) > 0 {
		err = h.Execute()
	} else {
		err = h.ExecuteRead()
	}
	if err != nil {
		h.Close()
		return err
	}
	for i := range reads {
		r := &reads[i]
		if r.Err != nil {
			continue
		}
		r.N, r.Err = h.ReadInto(r.Addr, r.Dst)
		if r.Err == nil && r.N == 0 {
			r.Err = fmt.Errorf("%w: va=0x%X", gm.ErrReadFailed, r.Addr)
		}
	}
	b.release(h)
	return nil
}

// Info implements backend.ProcessInfo.
func (b *Backend) Info(pid uint32) (backend.ProcInfo, error) {
	pi, err := b.vmm.GetProcessInfo(pid)
	if err != nil {
		return backend.ProcInfo{}, err
	}
	return backend.ProcInfo{
		PID:   pid,
		X64:   pi.MemoryModel == gm.MemoryModelX64,
		Wow64: pi.Win.IsWow64 != 0,
	}, nil
}

// PidByName implements backend.ProcessInfo.
func (b *Backend) PidByName(name string) (uint32, error) { return b.vmm.GetPidByName(name) }

// ModuleBase implements backend.ProcessInfo.
func (b *Backend) ModuleBase(pid uint32, name string) (uint64, error) {
	return b.vmm.GetModuleBase(pid, name)
}
