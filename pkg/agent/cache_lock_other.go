//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package agent

import (
	"errors"
	"os"
)

func lockCacheFile(*os.File) (func(), error) {
	return nil, errors.New("agent: cache file locking unsupported on this platform")
}
