package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/workspaces"
)

// copyCreatedDestination resolves only the placement returned by db.create.
// Unlike databaseEndpoint, it must not fall back to the master or a cached
// placement for a newly created database.
func copyCreatedDestination(reply remote.Reply, name string, workspace *workspaces.Workspace) (config.ClusterSpec, dbEndpoint, error) {
	fail := func() (config.ClusterSpec, dbEndpoint, error) {
		return config.ClusterSpec{}, dbEndpoint{}, errors.New("could not verify the created database placement; the database may exist and must be inspected before retrying")
	}

	var result struct {
		Database config.ClusterSpec `json:"database"`
	}

	decoder := json.NewDecoder(bytes.NewReader(reply.Data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&result); err != nil {
		return fail()
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return fail()
	}

	db := result.Database
	if db.Name != name || db.Node == "" || db.Port < config.ClusterPortMin || db.Port > config.ClusterPortMax || workspace == nil {
		return fail()
	}

	var match *workspaces.Node

	for i := range workspace.Nodes {
		if workspace.Nodes[i].Name == db.Node {
			if match != nil {
				return fail()
			}

			match = &workspace.Nodes[i]
		}
	}

	if match == nil || net.ParseIP(match.WGAddress) == nil || strings.TrimSpace(match.Hostname) == "" || match.Hostname != strings.TrimSpace(match.Hostname) {
		return fail()
	}

	return db, dbEndpoint{WGAddress: match.WGAddress, Hostname: match.Hostname}, nil
}
