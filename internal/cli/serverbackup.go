package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/spf13/cobra"
)

// eru server backup now|list|verify|export (Phase 5): the master bundle.

func serverBackupCommands() cmdSpec {
	return cmdSpec{
		use: "backup",
		run: parentRun,
		meta: CommandMeta{
			Summary:   "Back up the master's own state (the master bundle)",
			WhenToUse: []string{"Check, take or copy the encrypted bundle that server init --restore rebuilds the master from. Databases are backed up by eru backup."},
			Examples:  []string{"eru server backup list"},
			Remote:    true,
		},
		children: []cmdSpec{
			{
				use: "now",
				run: runServerBackupNow,
				flags: []fl{
					flagBool("force", "Build a bundle even when nothing changed"),
				},
				meta: CommandMeta{
					Summary:        "Build and upload a master bundle now",
					WhenToUse:      []string{"Before a risky change, or to check the master backup works. Bundles are also made after every change and daily."},
					Examples:       []string{"eru server backup now", "eru server backup now --dry-run"},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes:     []string{"node_fenced", "input_required", "backup_auth_failed", "endpoint_unreachable"},
					Next:           []string{"eru server backup list"},
				},
			},
			{
				use: "list",
				run: runServerBackupList,
				flags: []fl{
					flagBool("local", "Only the bundles kept on the server"),
					flagBool("bucket", "Only the bundles in the bucket (works while the server is down)"),
				},
				meta: CommandMeta{
					Summary:   "List the master bundles in the bucket and on the server",
					WhenToUse: []string{"Pick a bundle for server init --restore --bundle, or check the newest is current."},
					Examples:  []string{"eru server backup list", "eru server backup list --bucket --json"},
					Remote:    true,
				},
			},
			{
				use: "verify",
				run: runServerBackupVerify,
				flags: []fl{
					flagString("bundle", "", "A bundle name or a time (default: the newest)"),
					flagString("bundle-file", "", "Verify an exported bundle file instead"),
				},
				meta: CommandMeta{
					Summary:   "Download, decrypt and check a master bundle on this machine",
					WhenToUse: []string{"Prove a rebuild would work: the bundle opens with the recovery identity and matches the server's live state."},
					Examples:  []string{"eru server backup verify", "eru server backup verify --bundle-file prod-master.tar.age"},
					Remote:    true,
					ErrorCodes: []string{
						"bundle_corrupt", "bundle_not_found", "secrets_error",
					},
				},
			},
			{
				use: "export",
				run: runServerBackupExport,
				flags: []fl{
					flagString("bundle", "", "A bundle name (default: the newest on the server)"),
					flagString("out", "", "Output path (default ./<bundle>.tar.age)"),
				},
				meta: CommandMeta{
					Summary:          "Copy a master bundle, still encrypted, to this machine",
					WhenToUse:        []string{"Without a bucket: keep a copy off the server, so server init --restore --bundle-file can rebuild it."},
					Examples:         []string{"eru server backup export --out prod-master.tar.age"},
					Mutating:         true,
					SkipDryRunCheck:  true,
					SkipDryRunReason: "export only writes a local file",
					Remote:           true,
					ErrorCodes:       []string{"bundle_not_found"},
					Next:             []string{"eru server backup verify --bundle-file <file>"},
				},
			},
		},
	}
}

func runServerBackupNow(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "master-backup.now", map[string]any{"force": flagTrue(cmd, "force")},
		remote.CallOptions{DryRun: dryRun(cmd), OnEvent: p.event})
	if err != nil {
		return err
	}
	var res node.MasterBackupResult
	if err := reply.Decode(&res); err != nil {
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: reply.Data, Message: "dry run: would build a master bundle if anything changed", Warnings: warnings})
	}
	msg := "the master state is unchanged since " + res.Name
	if res.Built {
		msg = "built master bundle " + res.Name
	}
	switch {
	case res.Uploaded && res.Built:
		msg += " and uploaded it to the bucket"
	case res.Uploaded:
		msg += "; the bucket lacked it, so it was uploaded"
	case res.Bucket && res.Verified:
		msg += "; the bucket has it"
	case res.Bucket:
		msg += "; it was uploaded before (the bucket wasn't checked this run)"
	default:
		msg += "; no bucket, so it stays on the server (eru server backup export)"
	}
	return output.Success(cmd, output.Result{Data: res, Message: msg, Warnings: warnings})
}

// masterListData is server backup list's result.
type masterListData struct {
	Bucket []backup.MasterBundle `json:"bucket,omitempty"`
	Local  []node.LocalBundle    `json:"local,omitempty"`
	Epoch  *masterbackup.Epoch   `json:"epoch,omitempty"`
	Status *masterStatusData     `json:"status,omitempty"`
}

// masterStatusData is master-backup.list's result as the operator reads it.
type masterStatusData struct {
	Last        node.MasterLast      `json:"last"`
	Local       []node.LocalBundle   `json:"local"`
	Epoch       masterbackup.Epoch   `json:"epoch"`
	InstanceID  string               `json:"instance_id,omitempty"`
	Fenced      *masterbackup.Fenced `json:"fenced,omitempty"`
	Dirty       bool                 `json:"dirty"`
	Bucket      bool                 `json:"bucket"`
	ContentHash string               `json:"content_hash,omitempty"`
}

func runServerBackupList(cmd *cobra.Command, _ []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	ctx := commandContext(cmd)
	var data masterListData
	var warnings []output.Warning
	wantBucket, wantLocal := !flagTrue(cmd, "local"), !flagTrue(cmd, "bucket")
	if wantBucket {
		if kit, err := recoveryKit(c.Name); err == nil {
			stop := waitOn(cmd, "listing the master bundles in "+kit.Bucket)
			data.Bucket, err = backup.MasterList(ctx, kitTarget(kit))
			if err == nil {
				data.Epoch, err = backup.ReadEpoch(ctx, kitTarget(kit))
			}
			stop()
			if err != nil && !wantLocal {
				return err
			}
			if err != nil {
				warnings = append(warnings, output.Warning{Code: "bucket_unreachable", Message: "the bucket can't be listed: " + err.Error()})
			}
		} else if !wantLocal {
			return err
		}
	}
	if wantLocal {
		p := newProgress(cmd)
		s, err := openSession(cmd, p)
		if err != nil {
			if !wantBucket {
				return err
			}
			warnings = append(warnings, output.Warning{Code: "server_unreachable", Message: "the server's own bundles can't be listed: " + err.Error()})
		} else {
			defer s.Close()
			reply, err := p.call(ctx, s.node, "master-backup.list", nil, remote.CallOptions{OnEvent: p.event})
			if err != nil {
				return err
			}
			var st masterStatusData
			if err := reply.Decode(&st); err != nil {
				return err
			}
			data.Status, data.Local = &st, st.Local
			warnings = append(warnings, s.warnings...)
		}
	}
	return output.Success(cmd, output.Result{Data: data, Message: masterListMessage(data), Warnings: warnings})
}

func masterListMessage(d masterListData) string {
	var lines []string
	if d.Epoch != nil {
		lines = append(lines, fmt.Sprintf("epoch %d, instance %s (written %s)", d.Epoch.Epoch, d.Epoch.InstanceID, d.Epoch.WrittenAt))
	}
	if d.Status != nil && d.Status.Fenced != nil {
		lines = append(lines, "this server is FENCED: a newer master took over its bucket")
	}
	if len(d.Bucket) > 0 {
		rows := [][]string{{"BUCKET", "EPOCH", "SIZE", "COMPLETE"}}
		for _, b := range d.Bucket {
			complete := "yes"
			if !b.Complete {
				complete = "no (no sidecar)"
			}
			rows = append(rows, []string{b.Name, fmt.Sprint(b.Epoch), humanBytes(b.Size), complete})
		}
		lines = append(lines, alignColumns(rows)...)
	}
	if len(d.Local) > 0 {
		uploaded := ""
		if d.Status != nil {
			uploaded = d.Status.Last.UploadedName
		}
		rows := [][]string{{"ON THE SERVER", "EPOCH", "SIZE", "UPLOADED"}}
		for _, b := range d.Local {
			up := ""
			if b.Name == uploaded {
				up = "yes"
			}
			rows = append(rows, []string{b.Name, fmt.Sprint(b.Epoch), humanBytes(b.Size), up})
		}
		lines = append(lines, alignColumns(rows)...)
	}
	if len(lines) == 0 {
		return "no master bundles yet"
	}
	return strings.Join(lines, "\n")
}

func runServerBackupVerify(cmd *cobra.Command, _ []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	_, identity, err := recoveryIdentity(c.Name)
	if err != nil {
		return err
	}
	ctx := commandContext(cmd)
	var f fetchedBundle
	if path := stringFlag(cmd, "bundle-file"); path != "" {
		f, err = readBundleFile(path, cmd.InOrStdin())
	} else {
		kit, kerr := recoveryKit(c.Name)
		if kerr != nil {
			return kerr
		}
		stop := waitOn(cmd, "downloading the master bundle")
		f, err = fetchBundleFromBucket(ctx, kit, stringFlag(cmd, "bundle"))
		stop()
	}
	if err != nil {
		return err
	}
	b, err := openFetched(f, identity, c.Server)
	if err != nil {
		return err
	}
	data := map[string]any{"bundle": f.Name, "from": f.From, "manifest": manifestSummary(b.Manifest), "state": "unknown"}
	message := fmt.Sprintf("master bundle %s opens and checks out: %d files, created %s, epoch %d", f.label(), len(b.Files), b.Manifest.CreatedAt, b.Manifest.Epoch)
	var warnings []output.Warning
	p := newProgress(cmd)
	if s, err := openSession(cmd, p); err == nil {
		defer s.Close()
		if reply, err := p.call(ctx, s.node, "master-backup.list", nil, remote.CallOptions{OnEvent: p.event}); err == nil {
			var st masterStatusData
			if reply.Decode(&st) == nil && st.ContentHash != "" {
				if st.ContentHash == b.Manifest.Content {
					data["state"] = "current"
					message += "\nit matches the server's live state (current)"
				} else {
					data["state"] = "behind"
					message += "\nthe server has changed since (behind): eru server backup now"
				}
			}
		}
	} else {
		warnings = append(warnings, output.Warning{Code: "server_unreachable", Message: "couldn't compare with the server: " + err.Error()})
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings})
}

// manifestSummary is a manifest without file hashes, for output.
func manifestSummary(m masterbackup.Manifest) map[string]any {
	var files []string
	for _, f := range m.Files {
		files = append(files, f.Path)
	}
	return map[string]any{"server": m.Server, "node": m.Node, "epoch": m.Epoch, "revision": m.Revision,
		"created_at": m.CreatedAt, "eru_version": m.EruVersion, "files": files}
}

func runServerBackupExport(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "master-backup.get", map[string]any{"name": stringFlag(cmd, "bundle"), "export": true},
		remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var got node.MasterBundleData
	if err := reply.Decode(&got); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(got.Bundle)
	if err != nil {
		return err
	}
	if got.Sidecar.SHA256 != "" && got.Sidecar.SHA256 != masterbackup.SHA256(raw) {
		return output.NewError(output.CodeBundleCorrupt, "the bundle changed in transit", output.ExitSecrets, "Run the export again.")
	}
	out := stringFlag(cmd, "out")
	if out == "" {
		out = got.Name + masterbackup.BundleSuffix
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	if err := writeNewFile(out, raw); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:    map[string]any{"bundle": got.Name, "out": out, "size": len(raw)},
		Message: fmt.Sprintf("exported master bundle %s (%s, still encrypted) to %s\nrebuild from it with eru server init --restore --bundle-file %s", got.Name, humanBytes(int64(len(raw))), out, out),
	})
}

// writeNewFile writes data to path with mode 0600, refusing an existing
// file.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator names the output.
	if err != nil {
		if os.IsExist(err) {
			return output.NewError(output.CodePrecondition, path+" already exists", output.ExitPrecondition, "Pass --out with another path.").
				WithDetails(map[string]any{"reason": "out_exists", "path": path})
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
