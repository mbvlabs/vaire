//go:build !linux

package backup

// deviceOf is unused off Linux, where free space is never checked.
func deviceOf(string) uint64 { return 0 }
