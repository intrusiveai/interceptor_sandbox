//go:build linux || darwin

package credentials

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func readPrivate(name string, maximum int64) ([]byte, error) {
	invalid := errors.New("private credential file unavailable")
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || maximum < 1 || maximum > 1<<20 {
		return nil, invalid
	}
	f, e := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, invalid
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return nil, invalid
	}
	st, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || st.Nlink != 1 || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || before.Size() > maximum {
		return nil, invalid
	}
	raw, e := io.ReadAll(io.LimitReader(f, maximum+1))
	if e != nil {
		return nil, invalid
	}
	after, e := f.Stat()
	if e != nil || after.Size() != before.Size() || after.Size() != int64(len(raw)) || int64(len(raw)) > maximum || !after.ModTime().Equal(before.ModTime()) {
		return nil, invalid
	}
	return raw, nil
}
