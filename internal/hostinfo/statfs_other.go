//go:build !linux

package hostinfo

// statfsGiB is unavailable off Linux; the operator never detects a data disk.
func statfsGiB(string) (int, bool) { return 0, false }

// FreeBytes is unavailable off Linux; only nodes check free space.
func FreeBytes(string) (uint64, bool) { return 0, false }
