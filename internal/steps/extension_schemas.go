package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/pg"
)

// InstalledExtensionSchemasSQL reads every installed extension's actual
// metadata namespace. The LEFT JOIN is deliberate: a row with no readable
// namespace is still installed and must remain visible as an unknown fact,
// not disappear and be mistaken for absence. It is metadata only; a built-in
// extension's owned objects or grant target may live in another schema (for
// example pg_cron uses pg_catalog here while its objects and grants use cron).
const InstalledExtensionSchemasSQL = `SELECT e.extname AS name, coalesce(n.nspname, '') AS schema
FROM pg_extension e
LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
ORDER BY 1`

// installedExtensionSchemasColumnSQL is the roles-step form read through
// queryColumn, so existing roles host-effect hooks still intercept one column
// and an unreadable namespace remains an empty known fact.
const installedExtensionSchemasColumnSQL = `SELECT e.extname || E'\x1f' || coalesce(n.nspname, '') AS identity
FROM pg_extension e
LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
ORDER BY 1`

// queryInstalledExtensionSchemasColumn is the roles step's read-only seam.
func queryInstalledExtensionSchemasColumn(ctx context.Context, local pg.Local, db string) (map[string]string, error) {
	rows, err := rolesQueryColumn(ctx, local, db, installedExtensionSchemasColumnSQL)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		name, schema, ok := strings.Cut(row, "\x1f")
		if !ok {
			name, schema = row, ""
		}
		if name == "" {
			return nil, fmt.Errorf("installed extension schema query in %s returned an empty extension name", db)
		}
		out[name] = schema
	}
	return out, nil
}

// QueryInstalledExtensionSchemas returns extname -> actual metadata namespace.
// An empty value means the row was present but its namespace was not readable.
func QueryInstalledExtensionSchemas(ctx context.Context, local pg.Local, db string) (map[string]string, error) {
	rows, err := local.Query(ctx, db, InstalledExtensionSchemasSQL, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		name := row["name"]
		if name == "" {
			return nil, fmt.Errorf("installed extension schema query in %s returned an empty extension name", db)
		}
		out[name] = row["schema"]
	}
	return out, nil
}

// ValidateInstalledExtensionSchemas refuses an installed SQL extension whose
// actual namespace contradicts a schema the descriptor explicitly targets.
// An absent extension is fresh: CREATE EXTENSION may still establish its
// namespace. A descriptor with no schema declares no expected namespace, so
// this validator never assumes public; callers that own a built-in grant or
// owned-object schema must verify that separately. An installed extension with
// no readable namespace fails closed.
func ValidateInstalledExtensionSchemas(db string, entries []catalog.Extension, installed map[string]string) error {
	for _, ext := range entries {
		if ext.ExtName == "" || ext.Schema == "" {
			continue
		}
		actual, ok := installed[ext.ExtName]
		if !ok {
			continue
		}
		if actual == "" {
			return output.NewError(output.CodePrecondition,
				"cannot prove the installed schema of "+ext.ExtName+" in "+db,
				output.ExitPrecondition,
				"Read the cluster catalog before applying extension SQL; vaire will not guess an installed namespace.").
				WithDetails(map[string]any{
					"reason": "extension_schema_unknown", "database": db,
					"extension": ext.Name, "ext_name": ext.ExtName,
				})
		}
		if actual != ext.Schema {
			return output.NewError(output.CodePrecondition,
				ext.Name+" is installed in "+db+" as SQL extension "+ext.ExtName+" in schema "+actual+
					", not the configured schema "+ext.Schema,
				output.ExitPrecondition,
				"Remove and re-add the extension with the configured schema, or correct cluster.toml; no extension SQL or grant was run.").
				WithDetails(map[string]any{
					"reason": "extension_schema_mismatch", "database": db,
					"extension": ext.Name, "ext_name": ext.ExtName,
					"actual_schema": actual, "requested_schema": ext.Schema,
				})
		}
	}
	return nil
}
