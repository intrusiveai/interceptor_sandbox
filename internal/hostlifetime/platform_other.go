//go:build !darwin

package hostlifetime

import "context"

type inertBackend struct{}
type inertInhibitor struct{}

func platformBackend() Backend                                { return inertBackend{} }
func (inertBackend) Start(context.Context) (Inhibitor, error) { return inertInhibitor{}, nil }
func (inertBackend) SleepMarker() (string, error)             { return "", nil }
func (inertInhibitor) Close()                                 {}
func (inertInhibitor) Done() <-chan struct{}                  { return nil }
