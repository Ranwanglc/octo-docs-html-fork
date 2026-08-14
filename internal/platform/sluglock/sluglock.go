// Package sluglock provides per-slug write serialization — an in-process keyed
// mutex. It makes a slug's read-modify-write of its comment list atomic within
// one process.
//
// The interface admits a future distributed implementation (e.g. a PostgreSQL
// advisory lock) for multi-instance deployments without changing callers.
package sluglock

import (
	"context"
	"sync"
)

// Locker serializes work per key.
type Locker interface {
	// With runs fn while holding the lock for key, releasing it afterward.
	With(ctx context.Context, key string, fn func() error) error
}

// LockSession holds one underlying locking session. Acquire adds keys in the
// caller's order; Session releases all acquired keys when its callback returns.
type LockSession interface {
	Acquire(ctx context.Context, key string) error
}

// SessionLocker serializes a multi-key critical section on one underlying
// session. Advisory implementations consequently use only one pooled
// connection even when the callback acquires several keys.
type SessionLocker interface {
	Session(ctx context.Context, fn func(LockSession) error) error
}

// Memory is an in-process keyed mutex. Correct for the single-instance default.
type Memory struct {
	mu    sync.Mutex
	locks map[string]*entry
}

// entry is a per-key mutex plus a waiter count so idle keys can be reclaimed —
// without the count, the map would grow unbounded with every distinct slug ever
// seen (a memory leak for attacker-chosen slugs on a public server).
type entry struct {
	mu      sync.Mutex
	waiters int
}

// NewMemory creates an in-process keyed locker.
func NewMemory() *Memory {
	return &Memory{locks: make(map[string]*entry)}
}

// acquire returns the entry for key, incrementing its waiter count under the map
// lock so the entry can't be reclaimed while this caller waits for or holds it.
func (m *Memory) acquire(key string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.locks[key]
	if !ok {
		e = &entry{}
		m.locks[key] = e
	}
	e.waiters++
	return e
}

// release decrements the waiter count and deletes the entry when no one else is
// waiting on or holding it, keeping the map bounded to in-flight keys.
func (m *Memory) release(key string, e *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.waiters--
	if e.waiters == 0 {
		delete(m.locks, key)
	}
}

// With runs fn under the per-key lock. The context is honored before acquiring;
// once acquired, fn runs to completion.
func (m *Memory) With(ctx context.Context, key string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := m.acquire(key)
	defer m.release(key, e)
	e.mu.Lock()
	defer e.mu.Unlock()
	return fn()
}

type memorySession struct {
	m    *Memory
	held []memoryHeld
}

type memoryHeld struct {
	key string
	e   *entry
}

// Acquire locks keys in caller order. Canonical creation acquires idem then
// slug, while legacy callers only acquire slug, so that order has no cycle.
func (s *memorySession) Acquire(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := s.m.acquire(key)
	e.mu.Lock()
	s.held = append(s.held, memoryHeld{key: key, e: e})
	return nil
}

// Session runs fn and unlocks acquired keys in reverse order.
func (m *Memory) Session(ctx context.Context, fn func(LockSession) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := &memorySession{m: m}
	defer func() {
		for i := len(s.held) - 1; i >= 0; i-- {
			s.held[i].e.mu.Unlock()
			m.release(s.held[i].key, s.held[i].e)
		}
	}()
	return fn(s)
}

var _ SessionLocker = (*Memory)(nil)
