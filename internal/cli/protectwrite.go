package cli

import (
	"slices"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/spf13/cobra"
)

// protectedWriteCode asks for approval when a writing command starts on a
// protected database. It returns nil when db is not protected:
// reads, and writes to unprotected databases, need no code. what names the
// write in the messages ("writing to it"). status reads protect.status;
// note prints a line before the prompt. Without an interactive terminal,
// with --yes and in structured output it refuses, so an agent gets
// read-only access to a protected database. The node checks the code; a
// code never unlocks a later command.
func protectedWriteCode(cmd *cobra.Command, db, what string, status func() (protectStatusData, error), note func(string)) (map[string]string, error) {
	st, err := status()
	if err != nil {
		return nil, err
	}

	if !slices.Contains(st.Protected, db) {
		return nil, nil
	}

	if isFollowerStatus(st.Databases, db) {
		// A follower takes no writes: the node refuses a write code for it
		// and the write itself fails on the read-only cluster. Asking for a
		// code would suggest a code could make it writable (Phase 6).
		return nil, nil
	}

	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return nil, output.NewError(output.CodeDatabaseProtected,
			"database "+db+" is protected, so "+what+" needs human approval at a terminal", output.ExitUnsafe,
			"Reads need no approval: run it without --write. To write, run at an interactive terminal without --yes or structured output and provide an enrolled approval factor.").
			WithDetails(map[string]any{"reason": "write_requires_human", "database": db})
	}

	if !hasApprovalFactor(st.ApprovalFactors) {
		return nil, output.NewError(output.CodeDatabaseProtected,
			"database "+db+" is protected and "+what+" needs an approval factor available to this operator", output.ExitUnsafe,
			node.ExposureEnrollmentHint).
			WithDetails(map[string]any{"reason": "protect_not_initialized", "database": db})
	}

	note("Database " + db + " is protected: " + what + " needs approval.")

	return promptApprovalFactor(cmd, st.ApprovalFactors, "Approve writing to "+db+" with")
}

// isFollowerStatus reports whether db is a follower in a protect.status
// result. A follower's protection, if any, has no writable path (Phase 6).
func isFollowerStatus(dbs []node.ProtectedDatabase, db string) bool {
	_, follower := followerSource(dbs, db)
	return follower
}

// followerSource is the source db follows in a protect.status result, and
// whether db is a follower at all.
func followerSource(dbs []node.ProtectedDatabase, db string) (string, bool) {
	for _, d := range dbs {
		if d.Name == db && d.Follows != "" {
			return d.Follows, true
		}
	}

	return "", false
}

// refuseFollowerWrite wraps a protect.status reader so a writing session on a
// follower is refused before protectedWriteCode decides anything: a follower
// is a read-only standby, it takes no writes, and no code from the
// authenticator app could make it writable (Phase 6). It refuses an
// unprotected follower too, which protectedWriteCode would pass over.
func refuseFollowerWrite(status func() (protectStatusData, error), db string) func() (protectStatusData, error) {
	return func() (protectStatusData, error) {
		st, err := status()
		if err != nil {
			return protectStatusData{}, err
		}

		source, follower := followerSource(st.Databases, db)
		if follower {
			return protectStatusData{}, followerWriteError(db, source)
		}

		return st, nil
	}
}

// followerWriteError is the refusal a write meets on follower db: writes go to
// its source, or the follower can be made independent first.
func followerWriteError(db, source string) error {
	return output.NewError(output.CodePrecondition,
		db+" is a follower of "+source+", so writing to it is refused", output.ExitPrecondition,
		"Write to "+source+" instead, or make "+db+" independent first: vaire db unfollow "+db+" --confirm "+db+"/unfollow. Reads need no --write.").
		WithDetails(map[string]any{"reason": "is_follower", "database": db, "source": source})
}

// fetchWriteProtectStatus reads protect.status for a writing session. Tests
// replace it to stand in for the node's answer.
var fetchWriteProtectStatus = fetchProtectStatus

// sessionLogin is the login vaire sql, psql and console use on db: vaire_admin,
// or for a write to a protected database a short-lived role in <db>_owner
// that the node creates once the code from the authenticator app checks out
// (db.write-access). vaire_admin cannot write to a protected database, so the
// node enforces it whatever the client does. A write to a follower is refused
// here, before it asks for a code or acts as an owner role: a follower takes
// no writes.
func sessionLogin(cmd *cobra.Command, s *session, p *progress, db string, write bool) (string, string, error) {
	if !write {
		return adminCredentials(s.ctx)
	}

	status := func() (protectStatusData, error) { return fetchWriteProtectStatus(cmd, s, p) }

	code, err := protectedWriteCode(cmd, db, "writing to it", refuseFollowerWrite(status, db), p.note)
	if err != nil {
		return "", "", err
	}

	if code == nil {
		return adminCredentials(s.ctx)
	}
	// The CLI makes the write role's password and sends it as a secret:
	// the node redacts every secret from its result, so it cannot hand one
	// back (node.WritePasswordSecret).
	writePassword, err := secrets.Base62(32)
	if err != nil {
		return "", "", err
	}

	code[node.WritePasswordSecret] = writePassword

	reply, err := p.call(commandContext(cmd), s.node, "db.write-access", map[string]any{"db": db},
		remote.CallOptions{Secrets: code, OnEvent: p.event})
	if err != nil {
		return "", "", err
	}

	var access node.WriteAccess
	if err := reply.Decode(&access); err != nil {
		return "", "", err
	}

	if !access.Protected {
		// The protection was lifted meanwhile: vaire_admin writes again.
		return adminCredentials(s.ctx)
	}

	return access.User, writePassword, nil
}
