package render

// FollowerConfName is the conf.d file only a follower has (Phase 6): its
// hot-standby floors and its replication settings. It loads after vaire's
// other files, so its floors win over the follower's own tuning.
const FollowerConfName = "60-vaire-follower.conf"

// Floor is one hot-standby floor: a setting a standby must have at least as
// high as its primary.
type Floor struct {
	Name   string
	Value  string
	Reason string
}

// FollowerSettings is what 60-vaire-follower.conf says.
type FollowerSettings struct {
	// Name is the follower, Source the database it follows.
	Name, Source string
	Floors       []Floor
	// Conninfo is primary_conninfo, Slot primary_slot_name.
	Conninfo, Slot string
	// RestoreCommand fetches WAL from the source's stanza; empty without
	// backups.
	RestoreCommand string
	// Timeline is recovery_target_timeline when restore undo recorded one
	// for the source's stanza; empty is PostgreSQL's latest.
	Timeline string
}

// FollowerConf renders a follower's conf.d/60-vaire-follower.conf.
func FollowerConf(f FollowerSettings) []byte { return execute("60-vaire-follower.conf.tmpl", f) }
