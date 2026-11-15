package node

import "testing"

// mustAs asserts value's dynamic type, failing the test instead of panicking.
// Tests use it for result envelopes so an unexpected shape reports the test
// and the offending value.
func mustAs[T any](t *testing.T, value any) T {
	t.Helper()
	typed, ok := value.(T)
	rejectf(t, !ok, "value has type %T, want %T", value, typed)
	return typed
}

// reject fails the test when cond is true, with t.Fatal semantics.
func reject(t *testing.T, cond bool, args ...any) {
	t.Helper()
	if cond {
		t.Fatal(args...)
	}
}

// rejectf fails the test when cond is true, with t.Fatalf formatting.
func rejectf(t *testing.T, cond bool, format string, args ...any) {
	t.Helper()
	if cond {
		t.Fatalf(format, args...)
	}
}

// mustNoErr fails the test when err is non-nil. Setup steps use it when the
// error itself is the useful diagnostic.
func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
