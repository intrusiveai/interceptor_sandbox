package hostlifetime

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeBackend struct {
	mu             sync.Mutex
	marker         string
	err            error
	starts, closes int
	inhibitor      *fakeInhibitor
}
type fakeInhibitor struct {
	backend *fakeBackend
	done    chan struct{}
	once    sync.Once
}

func (f *fakeBackend) Start(context.Context) (Inhibitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.starts++
	f.inhibitor = &fakeInhibitor{backend: f, done: make(chan struct{})}
	return f.inhibitor, nil
}
func (f *fakeBackend) SleepMarker() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marker, f.err
}
func (f *fakeInhibitor) Close() {
	f.once.Do(func() { f.backend.mu.Lock(); f.backend.closes++; f.backend.mu.Unlock(); close(f.done) })
}
func (f *fakeInhibitor) Done() <-chan struct{} { return f.done }
func TestRestoreLeaseKeepsAssertionAcrossTargets(t *testing.T) {
	backend := &fakeBackend{}
	m := NewWithBackend(backend, nil)
	source, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	restore, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source()
	source()
	if backend.closes != 0 {
		t.Fatal("released while restoring")
	}
	replacement, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	restore()
	if backend.starts != 1 || backend.closes != 0 {
		t.Fatal("assertion gap during restore")
	}
	replacement()
	if backend.closes != 1 {
		t.Fatal("idle API retained assertion")
	}
}
func TestSleepChangePermanentlyClosesActiveWork(t *testing.T) {
	backend := &fakeBackend{marker: "before"}
	faults := 0
	m := NewWithBackend(backend, func(err error) {
		faults++
		if err.Error() != "HOST_SLEEP_INTERRUPTED" {
			t.Error(err)
		}
	})
	release, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	backend.mu.Lock()
	backend.marker = "after"
	backend.mu.Unlock()
	if err := m.Check(); !IsInterruption(err) {
		t.Fatal(err)
	}
	_ = m.Check()
	if faults != 1 {
		t.Fatal("repeated fault", faults)
	}
	if _, err := m.Acquire(context.Background()); err == nil {
		t.Fatal("reopened interrupted execution")
	}
}
func TestMissingAssertionAndUnreadableSleepState(t *testing.T) {
	for _, mode := range []string{"helper", "marker"} {
		t.Run(mode, func(t *testing.T) {
			backend := &fakeBackend{}
			m := NewWithBackend(backend, nil)
			release, err := m.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if mode == "helper" {
				backend.inhibitor.Close()
			} else {
				backend.mu.Lock()
				backend.err = errors.New("unavailable")
				backend.mu.Unlock()
			}
			if err := m.Check(); !IsInterruption(err) {
				t.Fatal("missing protection accepted", err)
			}
		})
	}
}
func TestFailedStartupDoesNotAcquireLease(t *testing.T) {
	backend := &fakeBackend{err: errors.New("denied")}
	m := NewWithBackend(backend, nil)
	if _, err := m.Acquire(context.Background()); err == nil {
		t.Fatal("startup succeeded without protection")
	}
	if m.refs != 0 {
		t.Fatal("leaked lease")
	}
}
