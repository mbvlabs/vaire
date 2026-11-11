package config

import (
	"slices"
	"sort"
)

// Followers (Phase 6): a [[clusters]] entry with follows = "<source>" is a
// read-only physical standby of the source's cluster, a cluster of its own
// with its own size and port. Inside it the database and roles are the
// source's, so the database it opens is the source's name.

// IsFollower reports whether spec follows another database.
func (s ClusterSpec) IsFollower() bool { return s.Follows != "" }

// DatabaseName is the database inside spec's cluster: the source's name for
// a follower, its own otherwise.
func (s ClusterSpec) DatabaseName() string {
	if s.Follows != "" {
		return s.Follows
	}
	return s.Name
}

// FollowerSlot is the physical replication slot a follower streams through
// on its source: vaire_<follower>, at most 61 characters (a database name is
// at most 55, so its role names fit in 63).
func FollowerSlot(follower string) string { return FollowerSlotPrefix + follower }

// FollowerSlotPrefix starts every slot vaire makes for a follower.
const FollowerSlotPrefix = "vaire_"

// FollowersOf lists the entries that follow name, in [[clusters]] order.
func (c Cluster) FollowersOf(name string) []ClusterSpec {
	var out []ClusterSpec
	for _, s := range c.Clusters {
		if s.Follows == name {
			out = append(out, s)
		}
	}
	return out
}

// FollowerNames lists the names of the entries that follow name.
func (c Cluster) FollowerNames(name string) []string {
	var out []string
	for _, s := range c.FollowersOf(name) {
		out = append(out, s.Name)
	}
	return out
}

// StanzaClusters are the entries with a stanza of their own: every database
// but the followers, whose source's backups cover them.
func (c Cluster) StanzaClusters() []ClusterSpec {
	out := make([]ClusterSpec, 0, len(c.Clusters))
	for _, s := range c.Clusters {
		if !s.IsFollower() {
			out = append(out, s)
		}
	}
	return out
}

// validateFollower checks a follower's entry: its source is another entry
// on the same node that follows nothing, and it has no stanza and no
// [clusters.backup].
func (c Cluster) validateFollower(add func(string, ...any), at string, s ClusterSpec) {
	src, ok := c.ClusterByName(s.Follows)
	switch {
	case s.Follows == s.Name:
		add("%s.follows %q can't name the database itself", at, s.Follows)
	case !ok:
		add("%s.follows %q is not a database in cluster.toml", at, s.Follows)
	case src.IsFollower():
		add("%s.follows %q is itself a follower; follow its source %q instead", at, s.Follows, src.Follows)
	case src.Node != s.Node:
		add("%s.follows %q is on node %q; a follower runs on its source's node until Phase 10", at, s.Follows, src.Node)
	}
	if ok && !src.IsFollower() {
		if !sameExplicitExtensions(c, s.Extensions, src.Extensions) {
			add("%s.extensions must equal its source %s's inherited list; edit %s, not the follower", at, s.Follows, s.Follows)
		}
		if !sameRecoveryDeclaration(c, s.RecoveryExtensions, src.RecoveryExtensions) {
			add("%s.recovery_extensions must equal its source %s's declaration; edit %s, not the follower", at, s.Follows, s.Follows)
		}
	}
	if s.Stanza != "" {
		add("%s.stanza %q must be empty for a follower: its source's backups cover it", at, s.Stanza)
	}
	if s.Backup != (ClusterBackup{}) {
		add("%s.backup must be empty for a follower: it has no stanza of its own", at)
	}
	if s.FollowGeneration != "" && !followGenerationPattern.MatchString(s.FollowGeneration) {
		add("%s.follow_generation %q is managed by vaire and must be 32 lowercase hexadecimal characters", at, s.FollowGeneration)
	}
}

// IsFollowerName reports whether name is a follower in c.
func (c Cluster) IsFollowerName(name string) bool {
	s, ok := c.ClusterByName(name)
	return ok && s.IsFollower()
}

// Sources are the names the followers in c follow, sorted, each once.
func (c Cluster) Sources() []string {
	var out []string
	for _, s := range c.Clusters {
		if s.IsFollower() && !slices.Contains(out, s.Follows) {
			out = append(out, s.Follows)
		}
	}
	slices.Sort(out)
	return out
}

func canonicalExplicit(c Cluster, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		canonical, _ := c.ExtensionResolver().Canonical(name)
		out = append(out, canonical)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func sameExplicitExtensions(c Cluster, a, b []string) bool {
	return slices.Equal(canonicalExplicit(c, a), canonicalExplicit(c, b))
}

func sameRecoveryDeclaration(c Cluster, a, b *[]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return slices.Equal(canonicalExplicit(c, *a), canonicalExplicit(c, *b))
}
