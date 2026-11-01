package smartmem_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend"
	"github.com/sergeyzav/smartmem/backend/fake"
)

func TestProcess(t *testing.T) {
	m := fake.New()
	m.Proc(1).Name("app.exe")
	m.Proc(2).Name("legacy.exe").Wow64()
	s := smartmem.New(m)

	p, err := s.Process(1)
	if err != nil || p.PID() != 1 || p.PtrSize() != 8 {
		t.Fatalf("Process(1) = %v, %v", p, err)
	}
	if p, err := s.ProcessByName("APP.exe"); err != nil || p.PID() != 1 {
		t.Errorf("ProcessByName = %v, %v", p, err)
	}

	_, err = s.Process(2)
	if !errors.Is(err, smartmem.ErrUnsupported) || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("WoW64 process: err=%v, want ErrUnsupported", err)
	}
	if _, err := s.Process(3); !errors.Is(err, fake.ErrNoProcess) {
		t.Errorf("unknown pid: err=%v, want the backend error wrapped", err)
	}
	if _, err := s.ProcessByName("none.exe"); !errors.Is(err, fake.ErrNoProcess) {
		t.Errorf("unknown name: err=%v", err)
	}
}

func TestModuleBaseIsCached(t *testing.T) {
	m := fake.New()
	fp := m.Proc(1).Module("core.dll", 0x7ff600000000, 0x1000)
	p, err := smartmem.New(m).Process(1)
	if err != nil {
		t.Fatal(err)
	}

	if base, err := p.ModuleBase("Core.DLL"); err != nil || base != 0x7ff600000000 {
		t.Fatalf("ModuleBase = %#x, %v", base, err)
	}
	fp.Module("core.dll", 0x7ff700000000, 0x1000) // the module "reloads" elsewhere
	if base, _ := p.ModuleBase("core.dll"); base != 0x7ff600000000 {
		t.Errorf("after reload without Refresh: %#x, want the cached base", base)
	}
	p.Refresh()
	if base, _ := p.ModuleBase("core.dll"); base != 0x7ff700000000 {
		t.Errorf("after Refresh: %#x, want the new base", base)
	}

	_, err = p.ModuleBase("missing.dll")
	var me *smartmem.ModuleError
	if !errors.As(err, &me) || me.Name != "missing.dll" || !errors.Is(err, fake.ErrNoModule) {
		t.Errorf("missing module: err=%v, want *ModuleError wrapping ErrNoModule", err)
	}
}

// execOnly is a backend without process lookups.
type execOnly struct{}

func (execOnly) Exec(context.Context, uint32, uint64, []backend.WriteOp, []backend.ReadOp) error {
	return nil
}

func TestBackendWithoutProcessInfo(t *testing.T) {
	s := smartmem.New(execOnly{})
	p, err := s.Process(7)
	if err != nil {
		t.Fatalf("Process: %v, want x64 assumed", err)
	}
	if _, err := s.ProcessByName("x.exe"); !errors.Is(err, smartmem.ErrUnsupported) {
		t.Errorf("ProcessByName: err=%v, want ErrUnsupported", err)
	}
	if _, err := p.ModuleBase("x.dll"); !errors.Is(err, smartmem.ErrUnsupported) {
		t.Errorf("ModuleBase: err=%v, want ErrUnsupported", err)
	}
	if _, err := p.PEB(); !errors.Is(err, smartmem.ErrUnsupported) {
		t.Errorf("PEB: err=%v, want ErrUnsupported", err)
	}
}

func TestPEB(t *testing.T) {
	m := fake.New()
	m.Proc(100).PEB(0x10000).U64(0x10018, 0x20000)
	m.Proc(200)
	s := smartmem.New(m)
	p, err := s.Process(100)
	if err != nil {
		t.Fatal(err)
	}
	peb, err := p.PEB()
	if err != nil {
		t.Fatal(err)
	}
	b := s.NewBatch()
	ldr := smartmem.Read[uint64](b, peb.Add(0x18))
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, err := ldr.Get(); err != nil || v != 0x20000 {
		t.Fatalf("PEB.Ldr = %#x, %v", v, err)
	}
	q, _ := s.Process(200)
	if _, err := q.PEB(); !errors.Is(err, smartmem.ErrUnsupported) {
		t.Fatalf("unknown PEB: err=%v, want ErrUnsupported", err)
	}
}
