// Package guide embeds the agent guide printed by vaire guide agents.
package guide

import _ "embed"

//go:embed agents.md
var Agents string
