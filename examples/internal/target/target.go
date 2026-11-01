// Package target opens a MemProcFS target for the example programs.
//
// The flags default to the environment variables of the integration tests,
// so the examples run against the same dump:
//
//	VMM_DLL_PATH=/path/to/vmm.so MEMDUMP_PATH=/path/to/memdump.raw go run ./examples/modules
package target

import (
	"errors"
	"flag"
	"fmt"
	"os"

	gm "github.com/sergeyzav/gomemprocfs"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/memprocfs"
)

var (
	lib    = flag.String("vmm", os.Getenv("VMM_DLL_PATH"), "path to the MemProcFS native library (vmm.dll, vmm.so, vmm.dylib)")
	device = flag.String("device", os.Getenv("MEMDUMP_PATH"), `MemProcFS device: a memory dump file, "fpga", ...`)
	proc   = flag.String("proc", "explorer.exe", "target process name")
)

// Target is an open MemProcFS session with one target process.
type Target struct {
	Vmm *gm.Vmm
	S   *smartmem.Session
	P   *smartmem.Process

	be *memprocfs.Backend
}

// Open opens the target described by the flags. Call it after flag.Parse.
func Open(opts ...smartmem.Option) (*Target, error) {
	if *lib == "" || *device == "" {
		return nil, errors.New("set -vmm and -device (or VMM_DLL_PATH and MEMDUMP_PATH)")
	}
	vmm, err := gm.NewVmm(*lib, gm.WithDevice(*device), gm.WithWaitInitialize())
	if err != nil {
		return nil, fmt.Errorf("open MemProcFS: %w", err)
	}
	be := memprocfs.New(vmm)
	s := smartmem.New(be, opts...)
	p, err := s.ProcessByName(*proc)
	if err != nil {
		be.Close()
		_ = vmm.Close()
		return nil, fmt.Errorf("process %s: %w", *proc, err)
	}
	return &Target{Vmm: vmm, S: s, P: p, be: be}, nil
}

// Close releases the backend and the MemProcFS session.
func (t *Target) Close() {
	t.be.Close()
	_ = t.Vmm.Close()
}
