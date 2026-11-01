package smartmem

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sergeyzav/smartmem/backend"
	"github.com/sergeyzav/smartmem/internal/engine"
)

// Session is the entry point: it owns a Backend and hands out Processes.
// It is safe for concurrent use.
type Session struct {
	be   backend.Backend
	info backend.ProcessInfo // nil if the backend has no process lookups
	opts options
	runs sync.Pool // *engine.Run
}

// New returns a Session over be.
func New(be backend.Backend, opts ...Option) *Session {
	s := &Session{be: be, opts: defaultOptions()}
	for _, o := range opts {
		o(&s.opts)
	}
	s.info, _ = be.(backend.ProcessInfo)
	return s
}

func (s *Session) engineOptions() engine.Options {
	return engine.Options{MaxRounds: s.opts.maxRounds, MaxGap: uint64(s.opts.maxGap)}
}

func (s *Session) getRun() *engine.Run {
	if r, ok := s.runs.Get().(*engine.Run); ok {
		r.Reset(s.be, s.engineOptions())
		return r
	}
	return engine.NewRun(s.be, s.engineOptions())
}

func (s *Session) putRun(r *engine.Run) { s.runs.Put(r) }

// Process returns the process with this pid. In v1 only native x64 processes
// are supported: others return ErrUnsupported. If the backend cannot describe
// processes, the process is assumed to be x64.
func (s *Session) Process(pid uint32) (*Process, error) {
	if s.info != nil {
		pi, err := s.info.Info(pid)
		if err != nil {
			return nil, fmt.Errorf("smartmem: process %d: %w", pid, err)
		}
		if !pi.X64 || pi.Wow64 {
			return nil, fmt.Errorf("smartmem: process %d is not a native x64 process: %w", pid, ErrUnsupported)
		}
	}
	return &Process{s: s, pid: pid, ptrSize: 8, modules: make(map[string]uint64)}, nil
}

// ProcessByName finds a process by name and returns it as Process does.
func (s *Session) ProcessByName(name string) (*Process, error) {
	if s.info == nil {
		return nil, fmt.Errorf("smartmem: process %q: backend has no process lookups: %w", name, ErrUnsupported)
	}
	pid, err := s.info.PidByName(name)
	if err != nil {
		return nil, fmt.Errorf("smartmem: process %q: %w", name, err)
	}
	return s.Process(pid)
}

// Process is a target process. It builds locations and caches module bases.
// It is safe for concurrent use.
type Process struct {
	s       *Session
	pid     uint32
	ptrSize int

	mu      sync.RWMutex
	modules map[string]uint64 // lower-case name -> base
}

// PID returns the process id.
func (p *Process) PID() uint32 { return p.pid }

// PtrSize returns the pointer size in bytes. It is always 8 in v1.
func (p *Process) PtrSize() int { return p.ptrSize }

// Refresh drops cached module bases, for example after a module was reloaded.
func (p *Process) Refresh() {
	p.mu.Lock()
	clear(p.modules)
	p.mu.Unlock()
}

// moduleBase resolves a module base through the backend and caches it.
// Names are case-insensitive. Failures are not cached.
func (p *Process) moduleBase(name string) (uint64, error) {
	key := strings.ToLower(name)
	p.mu.RLock()
	base, ok := p.modules[key]
	p.mu.RUnlock()
	if ok {
		return base, nil
	}
	if p.s.info == nil {
		return 0, &ModuleError{Name: name, Err: ErrUnsupported}
	}
	base, err := p.s.info.ModuleBase(p.pid, name)
	if err != nil {
		return 0, &ModuleError{Name: name, Err: err}
	}
	p.mu.Lock()
	p.modules[key] = base
	p.mu.Unlock()
	return base, nil
}
