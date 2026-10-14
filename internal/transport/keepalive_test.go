package transport

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a settable last-read time.
type clock struct{ ns atomic.Int64 }

func (c *clock) touch()         { c.ns.Store(time.Now().UnixNano()) }
func (c *clock) get() time.Time { return time.Unix(0, c.ns.Load()) }
func newClock() *clock          { c := &clock{}; c.touch(); return c }
func runKeepalive(stop chan struct{}, silence time.Duration, probe func() error, last func() time.Time) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- keepalive(stop, 5*time.Millisecond, silence, probe, last) }()
	return out
}

func TestKeepaliveSlowRepliesAreNotLost(t *testing.T) {
	// Every reply takes four intervals, longer than the old per-probe
	// window, but replies keep arriving: the node is busy, not gone.
	c := newClock()
	var probes, inFlight, maxInFlight atomic.Int32
	probe := func() error {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		probes.Add(1)
		time.Sleep(20 * time.Millisecond)
		c.touch()
		inFlight.Add(-1)
		return nil
	}
	stop := make(chan struct{})
	done := runKeepalive(stop, 50*time.Millisecond, probe, c.get)
	time.Sleep(300 * time.Millisecond)
	close(stop)
	if lost := <-done; lost {
		t.Fatal("a node with slow replies was declared lost")
	}
	if probes.Load() < 3 {
		t.Fatalf("probes = %d, want several", probes.Load())
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("max probes in flight = %d, want 1", maxInFlight.Load())
	}
}

func TestKeepaliveChannelDataKeepsAlive(t *testing.T) {
	// The probe never returns, but channel data keeps arriving.
	c := newClock()
	block := make(chan struct{})
	defer close(block)
	var probes atomic.Int32
	probe := func() error { probes.Add(1); <-block; return errors.New("closed") }
	stop := make(chan struct{})
	done := runKeepalive(stop, 50*time.Millisecond, probe, c.get)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		c.touch()
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	if lost := <-done; lost {
		t.Fatal("a connection with inbound data was declared lost")
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1: never more than one in flight", probes.Load())
	}
}

func TestKeepaliveSilenceIsLost(t *testing.T) {
	c := newClock()
	block := make(chan struct{})
	defer close(block)
	probe := func() error { <-block; return nil }
	stop := make(chan struct{})
	defer close(stop)
	select {
	case lost := <-runKeepalive(stop, 50*time.Millisecond, probe, c.get):
		if !lost {
			t.Fatal("keepalive ended without reporting the silent connection lost")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a silent connection was never declared lost")
	}
}

func TestKeepaliveProbeErrorEnds(t *testing.T) {
	c := newClock()
	stop := make(chan struct{})
	defer close(stop)
	select {
	case lost := <-runKeepalive(stop, time.Minute, func() error { return errors.New("closed") }, c.get):
		if lost {
			t.Fatal("a closed connection was reported as lost to silence")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not end after the probe failed")
	}
}

func TestReadTrackerRecordsReads(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	tr := newReadTracker(a)
	before := tr.lastRead()
	time.Sleep(2 * time.Millisecond)
	go func() { _, _ = b.Write([]byte("x")) }()
	buf := make([]byte, 1)
	if _, err := tr.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !tr.lastRead().After(before) {
		t.Fatal("a read did not advance the last-read time")
	}
}
