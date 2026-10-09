// Package guide embeds the agent guide printed by eru guide agents.
package guide

import _ "embed"

//go:embed agents.md
var Agents string
