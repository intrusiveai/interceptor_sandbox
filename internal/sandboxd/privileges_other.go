//go:build !linux

package sandboxd

func adoptSupervisorIdentity(_, _ int) error { return nil }
func dropLaunchCapabilities() error          { return nil }
