package node

import "testing"

// nearestExisting returns the data root when it exists, else its nearest
// parent that does: on a server with no database the data root isn't made
// yet, and doctor measures the filesystem it will live on.
func TestNearestExisting(t *testing.T) {
	existing := map[string]bool{"/": true, "/var": true, "/var/lib": true}
	exists := func(p string) bool { return existing[p] }
	if got := nearestExisting("/var/lib/eru/clusters", exists); got != "/var/lib" {
		t.Errorf("missing tree: %q, want /var/lib", got)
	}
	existing["/var/lib/eru/clusters"] = true
	if got := nearestExisting("/var/lib/eru/clusters", exists); got != "/var/lib/eru/clusters" {
		t.Errorf("existing: %q", got)
	}
	// Nothing but the root exists.
	if got := nearestExisting("/nope/x", func(p string) bool { return p == "/" }); got != "/" {
		t.Errorf("only root: %q", got)
	}
}
