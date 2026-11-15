//go:build linux

package node

import "golang.org/x/sys/unix"

func statusMonotonicNS() (int64, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, false
	}
	ns := ts.Nano()
	return ns, ns > 0
}
