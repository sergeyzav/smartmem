package smartmem

// LocOffsets exposes the internal steps of a Loc to tests.
func LocOffsets(l Loc) []int64 { return l.offs }

// ModuleBase exposes module resolution to tests until the engine uses it.
func (p *Process) ModuleBase(name string) (uint64, error) { return p.moduleBase(name) }
