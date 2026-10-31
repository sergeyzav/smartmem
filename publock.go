package smartmem

import "sync"

// publishLock is a reader-preferring read/write lock. Unlike sync.RWMutex, a
// reader may take it again while holding it, even if a writer waits, so a
// Future's Get works inside Plan.View.
type publishLock struct {
	mu      sync.Mutex
	cond    sync.Cond
	readers int
	writer  bool
}

func (l *publishLock) RLock() {
	l.mu.Lock()
	for l.writer {
		l.cond.Wait()
	}
	l.readers++
	l.mu.Unlock()
}

func (l *publishLock) RUnlock() {
	l.mu.Lock()
	l.readers--
	if l.readers == 0 {
		l.cond.Broadcast()
	}
	l.mu.Unlock()
}

func (l *publishLock) Lock() {
	l.mu.Lock()
	for l.writer || l.readers > 0 {
		l.cond.Wait()
	}
	l.writer = true
	l.mu.Unlock()
}

func (l *publishLock) Unlock() {
	l.mu.Lock()
	l.writer = false
	l.cond.Broadcast()
	l.mu.Unlock()
}
