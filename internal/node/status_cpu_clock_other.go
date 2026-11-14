//go:build !linux

package node

func statusMonotonicNS() (int64, bool) { return 0, false }
