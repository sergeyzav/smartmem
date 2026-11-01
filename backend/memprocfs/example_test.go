package memprocfs_test

import (
	"context"
	"fmt"
	"log"

	gm "github.com/sergeyzav/gomemprocfs"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/memprocfs"
)

// Connect to MemProcFS, here over an FPGA device, and read a value from a
// process. The MemProcFS libraries must be installed; see the gomemprocfs
// README.
func Example() {
	vmm, err := gm.NewVmm("/path/to/vmm.so", gm.WithDevice("fpga"), gm.WithWaitInitialize())
	if err != nil {
		log.Fatal(err)
	}
	defer vmm.Close()
	be := memprocfs.New(vmm)
	defer be.Close() // before vmm.Close

	s := smartmem.New(be)
	p, err := s.ProcessByName("app.exe")
	if err != nil {
		log.Fatal(err)
	}
	v, err := smartmem.ReadNow[uint32](context.Background(), p.Module("core.dll").Add(0x1A2B30))
	fmt.Println(v, err)
}
