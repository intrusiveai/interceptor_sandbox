//go:build !linux

package sandboxd

import (
	"errors"
	"io"
	"os/exec"
)

func attachPTY(_ *exec.Cmd) (io.WriteCloser, io.ReadCloser, func(), error) {
	return nil, nil, nil, errors.New("PTY application interfaces are supported in the Linux sandbox container only")
}
