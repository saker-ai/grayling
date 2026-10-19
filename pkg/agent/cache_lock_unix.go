//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package agent

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockCacheFile(file *os.File) (func(), error) {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, ErrCacheBusy
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }, nil
}
