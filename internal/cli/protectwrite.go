package cli

import (
	"slices"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/spf13/cobra"
)

// protectedWriteCode asks once, when a writing command starts, for a code
// from the authenticator app when db is protected, and returns it as the
// request's protect_code secret. It returns nil when db is not protected:
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
	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return nil, output.NewError(output.CodeDatabaseProtected,
			"database "+db+" is protected, so "+what+" needs a code from the authenticator app, typed at a terminal", output.ExitUnsafe,
			"Reads need no code: run it without --write. A person who means to write runs the command at an interactive terminal, without --yes or structured output, and types the current 6-digit code from the authenticator app set up with vaire protect init.").
			WithDetails(map[string]any{"reason": "write_requires_human", "database": db})
	}
	if !st.Configured {
		return nil, output.NewError(output.CodeDatabaseProtected,
			"database "+db+" is protected and "+what+" needs two-factor protection, which this node does not have set up", output.ExitUnsafe,
			"A human runs vaire protect init first, at a terminal, and scans the QR code with an authenticator app.").
			WithDetails(map[string]any{"reason": "protect_not_initialized", "database": db})
	}
	if st.LockedUntil != "" {
		return nil, output.NewError(output.CodeProtectCodeRejected, "too many wrong codes; codes are locked out until "+st.LockedUntil, output.ExitUnsafe,
			"Wait until the lockout ends, then try again with a fresh code.").
			WithDetails(map[string]any{"reason": "locked", "locked_until": st.LockedUntil})
	}
	note("Database " + db + " is protected: " + what + " needs a code from the authenticator app set up with vaire protect init.")
	return readAuthenticatorCode(cmd, "nothing was written to "+db, map[string]any{"database": db})
}

// sessionLogin is the login vaire sql, psql and console use on db: vaire_admin,
// or for a write to a protected database a short-lived role in <db>_owner
// that the node creates once the code from the authenticator app checks out
// (db.write-access). vaire_admin cannot write to a protected database, so the
// node enforces it whatever the client does.
func sessionLogin(cmd *cobra.Command, s *session, p *progress, db string, write bool) (user, password string, err error) {
	user, password, err = adminCredentials(s.ctx)
	if err != nil || !write {
		return user, password, err
	}
	code, err := protectedWriteCode(cmd, db, "writing to it",
		func() (protectStatusData, error) { return fetchProtectStatus(cmd, s, p) }, p.note)
	if err != nil || code == nil {
		return user, password, err
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
		return user, password, nil
	}
	return access.User, writePassword, nil
}
