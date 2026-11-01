package smartmem_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sergeyzav/smartmem"
	"github.com/sergeyzav/smartmem/backend/fake"
)

const pid = 100

// env is a fake process 100 with a Session over it.
type env struct {
	mem *fake.Memory
	fp  *fake.Proc
	s   *smartmem.Session
	p   *smartmem.Process
	ctx context.Context
	t   *testing.T
}

func newEnv(t *testing.T, opts ...smartmem.Option) *env {
	t.Helper()
	m := fake.New()
	fp := m.Proc(pid)
	s := smartmem.New(m, opts...)
	p, err := s.Process(pid)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return &env{mem: m, fp: fp, s: s, p: p, ctx: ctx, t: t}
}

func (e *env) run(b *smartmem.Batch) {
	e.t.Helper()
	if err := b.Run(e.ctx); err != nil {
		e.t.Fatalf("Run: %v", err)
	}
}

// assertTrace compares the fake trace with want, ignoring indentation and
// blank lines, so expectations can be written as indented raw strings.
func (e *env) assertTrace(want string) {
	e.t.Helper()
	if got, w := normTrace(e.mem.Trace()), normTrace(want); got != w {
		e.t.Fatalf("trace:\n%s\nwant:\n%s", got, w)
	}
}

func normTrace(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

func get[T any](t *testing.T, f *smartmem.Future[T]) T {
	t.Helper()
	v, err := f.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return v
}

func getErr[T any](t *testing.T, f *smartmem.Future[T]) error {
	t.Helper()
	v, err := f.Get()
	if err == nil {
		t.Fatalf("Get = %v, want an error", v)
	}
	return err
}
