// Package devwait waits, bounded, for devices the host attached to appear.
//
// With an initramfs nothing waits for device probing before /init runs: a
// root-device boot waits for every pending probe before mounting root, an
// initramfs boot does not. virtio devices have so far always been probed in
// time, but vmsetup must not depend on that, so every device it uses is
// polled for first. devtmpfs creates the nodes itself; there is no udev.
package devwait

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Timeout bounds every wait. A device that has not appeared by then is not
// coming; the caller parks naming it. It costs nothing when the device is
// already there, which is the normal case.
const Timeout = 2 * time.Second

// interval is how often a missing device is checked for.
const interval = 2 * time.Millisecond

// Poll calls check until it returns nil or timeout passes, and then returns
// check's last error. check runs at least once.
func Poll(timeout time.Duration, check func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(interval)
	}
}

// Device waits for path to exist. It reports a device that never appeared
// as such, rather than leaving the caller to report a bare ENOENT from open
// or mount.
func Device(path string, timeout time.Duration) error {
	start := time.Now()
	err := Poll(timeout, func() error {
		_, err := os.Stat(path)
		return err
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("device %s did not appear within %s", path, time.Since(start).Round(time.Millisecond))
	default:
		return fmt.Errorf("device %s: %w", path, err)
	}
}
