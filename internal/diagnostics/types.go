// Package diagnostics defines the incident-bundle contract: one portable JSON
// document holding typed activity events and allowlisted metadata, small enough
// for an agent to inspect without an operator context or server credentials.
//
// A bundle never carries contexts, credentials, private keys, TOTP state,
// database dumps, configuration trees, raw service logs or free-form request,
// result and error bodies. Metadata is a fixed struct, not a map, Query is an
// optional typed activity.Query carrying only the filters used to select the
// page, and Activity is a typed activity.Page, so no open-ended payload can be
// added without changing this contract.
//
// Write creates the file owner-only and refuses a bundle larger than
// MaxBundleBytes. Read enforces the same cap and decodes with unknown fields
// rejected at every level, including the nested activity.Page and Query. Both
// validate Schema and the typed allowlist.
package diagnostics

import "github.com/mbvlabs/vaire/internal/activity"

// Schema versions the incident-bundle document. Later versions may only add
// optional fields.
const Schema = "vaire-incident/v1"

// MaxBundleBytes caps a bundle on write and read: 8 MiB.
const MaxBundleBytes int64 = 8 << 20

// Metadata identifies where and when a bundle was collected. All values are
// bounded identifiers or RFC3339 UTC timestamps; none is free-form diagnostic
// text.
type Metadata struct {
	Server        string `json:"server"`
	Node          string `json:"node"`
	NodeVersion   string `json:"node_version"`
	ClientVersion string `json:"client_version"`
	CollectedAt   string `json:"collected_at"`
}

// Bundle is one incident export. Schema is always Schema; CreatedAt is when the
// file was written, while Metadata.CollectedAt is when the evidence was read
// from the node.
//
// Query, when present, records the typed activity filters used to select
// Activity. It is operational metadata only: relative strings such as "24h"
// are preserved as the expression that was queried, alongside
// Metadata.CollectedAt. There are no resolver timestamps or arbitrary filter
// maps. Older bundles omit it and read as Query nil.
type Bundle struct {
	Schema    string          `json:"schema"`
	CreatedAt string          `json:"created_at"`
	Metadata  Metadata        `json:"metadata"`
	Query     *activity.Query `json:"query,omitempty"`
	Activity  activity.Page   `json:"activity"`
}
