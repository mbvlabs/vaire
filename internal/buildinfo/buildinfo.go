// Package buildinfo holds values stamped in at link time.
package buildinfo

// Version, Commit, and Channel are set with -ldflags. Dev builds skip
// signature checks and push ERU_NODE_BINARY when the operator OS differs.
var (
	Version = "dev"
	Commit  = "none"
	Channel = "dev"
)

// ReleaseBaseURL is the channel root for manifests and artifacts.
// Recorded in the decisions table in docs/MASTER.md.
const ReleaseBaseURL = "https://eru.mbvlabs.com"
