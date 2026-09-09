//go:build !linux

package serialport

import (
	"fmt"
	"runtime"
)

// Open is not implemented outside Linux.
//
// The exporter targets Linux, where its container runs and where the
// ch341-uart driver lives. This stub exists so the module still builds,
// vets and tests on other platforms: everything except the device layer is
// portable, and the protocol tests replay captured fixtures rather than
// touching hardware, so they run anywhere.
func Open(cfg Config) (Port, error) {
	return nil, fmt.Errorf(
		"serialport: opening %s is only supported on Linux, this binary was built for %s/%s",
		cfg.Path, runtime.GOOS, runtime.GOARCH)
}
