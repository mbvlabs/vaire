//go:build !linux

package hostinfo

// statfsGiB is unavailable off Linux; the operator never detects a data disk.
func statfsGiB(string) (int, bool) { return 0, false }
