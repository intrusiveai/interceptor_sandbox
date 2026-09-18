// Package hostlifetime protects active host work without extending idle API lifetime.
package hostlifetime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Interruption struct {
	Reason string `json:"reason"`
}

func (e *Interruption) Error() string { return e.Reason }
func Interrupted(reason string) error { return &Interruption{Reason: reason} }

// Platform implementations are inert on Linux. macOS supplies caffeinate and a
// kernel sleep/wake marker; no system power preferences are modified.
type Inhibitor interface {
	Close()
	Done() <-chan struct{}
}
type Backend interface {
	Start(context.Context) (Inhibitor, error)
	SleepMarker() (string, error)
}

type Manager struct {
	mu        sync.Mutex
	backend   Backend
	refs      int
	inhibitor Inhibitor
	marker    string
	fault     error
	cancel    context.CancelFunc
	onFault   func(error)
}

func New(onFault func(error)) *Manager { return NewWithBackend(platformBackend(), onFault) }
func NewWithBackend(backend Backend, onFault func(error)) *Manager {
	return &Manager{backend: backend, onFault: onFault}
}

// Acquire returns an idempotent release. A restore holds an extra lease so
// stopping the old target cannot briefly drop idle-sleep prevention.
func (m *Manager) Acquire(ctx context.Context) (func(), error) {
	m.mu.Lock()
	if m.fault != nil {
		err := m.fault
		m.mu.Unlock()
		return nil, err
	}
	if m.refs == 0 {
		marker, err := m.backend.SleepMarker()
		if err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("read host sleep state: %w", err)
		}
		inhibitor, err := m.backend.Start(ctx)
		if err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("start sleep prevention: %w", err)
		}
		m.marker = marker
		m.inhibitor = inhibitor
		// The assertion survives target cancellation until its shutdown cleanup ends.
		life, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		go m.watch(life)
	}
	m.refs++
	m.mu.Unlock()
	var once sync.Once
	return func() { once.Do(m.release) }, nil
}
func (m *Manager) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs--
	if m.refs == 0 {
		m.cancel()
		m.inhibitor.Close()
		m.inhibitor = nil
	}
}
func (m *Manager) watch(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.Check() != nil {
				return
			}
		}
	}
}

// Check is also called on host API admission; wake detection is not dependent
// on which goroutine is first scheduled after the machine wakes.
func (m *Manager) Check() error {
	m.mu.Lock()
	if m.refs == 0 {
		m.mu.Unlock()
		return nil
	}
	err := m.fault
	if err == nil {
		select {
		case <-m.inhibitor.Done():
			err = Interrupted("SLEEP_PREVENTION_LOST")
		default:
		}
	}
	if err == nil {
		marker, readErr := m.backend.SleepMarker()
		if readErr != nil {
			err = Interrupted("HOST_POWER_STATE_UNAVAILABLE")
		} else if marker != m.marker {
			err = Interrupted("HOST_SLEEP_INTERRUPTED")
		}
	}
	first := err != nil && m.fault == nil
	if first {
		m.fault = err
	}
	callback := m.onFault
	m.mu.Unlock()
	if first && callback != nil {
		callback(err)
	}
	return err
}
func IsInterruption(err error) bool {
	var interruption *Interruption
	return errors.As(err, &interruption)
}
