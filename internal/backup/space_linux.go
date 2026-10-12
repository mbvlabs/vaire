//go:build linux

package backup

import "syscall"

// deviceOf returns the device id of path's filesystem, so needs on one
// filesystem add up.
func deviceOf(path string) uint64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return st.Dev
}
