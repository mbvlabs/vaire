package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/minio/minio-go/v7"
)

// Long-retention dumps live at <prefix>/dumps/<db>/<id>.dump.age. The id is
// the UTC start time, with -final for the dump db drop takes.
const (
	dumpsDir    = "dumps/"
	dumpSuffix  = ".dump.age"
	dumpIDFmt   = "20060102T150405Z"
	finalSuffix = "-final"
	// dumpPartSize bounds the memory a streamed upload buffers, and with
	// S3's 10000 parts caps one dump at about 640 GiB.
	dumpPartSize = 64 << 20
)

// Dump is one long-retention dump in the bucket.
type Dump struct {
	Database string `json:"database"`
	ID       string `json:"id"`
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	Created  string `json:"created_at"`
	Final    bool   `json:"final,omitempty"`
}

// NewIdentity generates the age X25519 identity long-retention dumps are
// encrypted to. The operator keeps it; the node gets only the recipient.
func NewIdentity() (identity, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}

// DumpKey is the object key for a dump.
func (t Target) DumpKey(db, id string) string {
	return t.key(dumpsDir + db + "/" + id + dumpSuffix)
}

// NewDumpID returns the id for a dump started now.
func NewDumpID(final bool) string {
	id := time.Now().UTC().Format(dumpIDFmt)
	if final {
		id += finalSuffix
	}
	return id
}

// DumpToBucket streams pg_dump -Fc of db, encrypted to recipient, into the
// bucket. Nothing is written to the node's disk.
func DumpToBucket(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, t Target, src pg.Local, db, recipient string, final bool) (Dump, error) {
	rcpt, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return Dump{}, fmt.Errorf("the dump age recipient is invalid: %w", err)
	}
	c, err := t.client()
	if err != nil {
		return Dump{}, err
	}
	id := NewDumpID(final)
	key := t.DumpKey(db, id)
	pr, pw := io.Pipe()
	dumpErr := make(chan error, 1)
	go func() {
		// age writes its header on Encrypt, so it has to run here, once
		// PutObject below is reading the pipe.
		enc, err := age.Encrypt(pw, rcpt)
		if err == nil {
			err = PgDump(ctx, sh, stepID, report, src, db, enc)
			if err == nil {
				err = enc.Close()
			}
		}
		_ = pw.CloseWithError(err)
		dumpErr <- err
	}()
	info, putErr := c.PutObject(ctx, t.Bucket, key, pr, -1, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    dumpPartSize,
	})
	_ = pr.CloseWithError(putErr)
	if err := <-dumpErr; err != nil {
		if putErr == nil {
			_ = c.RemoveObject(context.WithoutCancel(ctx), t.Bucket, key, minio.RemoveObjectOptions{})
		}
		return Dump{}, err
	}
	if putErr != nil {
		return Dump{}, s3Error(t, "upload "+key, putErr)
	}
	return Dump{Database: db, ID: id, Key: key, Size: info.Size, Created: time.Now().UTC().Format(time.RFC3339), Final: final}, nil
}

// PgDump writes pg_dump -Fc of db on the instance at src to w.
func PgDump(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, src pg.Local, db string, w io.Writer) error {
	args := append([]string{"-u", "postgres", "--", "pg_dump", "-Fc", "--no-password"}, connArgs(src)...)
	args = append(args, "-d", db)
	return wrapTool("pg_dump "+db, sh.Exec(ctx, stepID, engine.Command{Name: "runuser", Args: args, Stdout: w}, report))
}

// ListDumps lists the dumps of db, or of every database when db is empty,
// oldest first.
func ListDumps(ctx context.Context, t Target, db string) ([]Dump, error) {
	rest := dumpsDir
	if db != "" {
		rest += db + "/"
	}
	objects, err := t.list(ctx, rest)
	if err != nil {
		return nil, err
	}
	base := t.key(dumpsDir)
	var out []Dump
	for _, o := range objects {
		rel := strings.TrimPrefix(o.Key, base)
		dbName, file, ok := strings.Cut(rel, "/")
		if !ok || !strings.HasSuffix(file, dumpSuffix) || strings.Contains(file, "/") {
			continue
		}
		id := strings.TrimSuffix(file, dumpSuffix)
		created := o.LastModified
		if t, err := time.Parse(dumpIDFmt, strings.TrimSuffix(id, finalSuffix)); err == nil {
			created = t.UTC().Format(time.RFC3339)
		}
		out = append(out, Dump{Database: dbName, ID: id, Key: o.Key, Size: o.Size, Created: created, Final: strings.HasSuffix(id, finalSuffix)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// PruneDumps deletes the oldest scheduled dumps of db beyond keep. Final
// dumps are never pruned.
func PruneDumps(ctx context.Context, t Target, db string, keep int) ([]string, error) {
	dumps, err := ListDumps(ctx, t, db)
	if err != nil {
		return nil, err
	}
	var scheduled []Dump
	for _, d := range dumps {
		if !d.Final {
			scheduled = append(scheduled, d)
		}
	}
	if keep < 1 || len(scheduled) <= keep {
		return nil, nil
	}
	c, err := t.client()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, d := range scheduled[:len(scheduled)-keep] {
		if err := c.RemoveObject(ctx, t.Bucket, d.Key, minio.RemoveObjectOptions{}); err != nil {
			return removed, s3Error(t, "delete "+d.Key, err)
		}
		removed = append(removed, d.ID)
	}
	return removed, nil
}

// OpenDump returns the decrypted pg_dump archive of one dump. The caller
// closes the reader.
func OpenDump(ctx context.Context, t Target, db, id, identity string) (io.ReadCloser, error) {
	ident, err := age.ParseX25519Identity(strings.TrimSpace(identity))
	if err != nil {
		return nil, output.NewError(output.CodeSecrets, "the dump age identity in the recovery kit is invalid", output.ExitSecrets,
			"Log in again from a current transfer bundle (eru login).")
	}
	c, err := t.client()
	if err != nil {
		return nil, err
	}
	key := t.DumpKey(db, id)
	if _, err := c.StatObject(ctx, t.Bucket, key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, output.NewError(output.CodePrecondition, "there is no dump "+id+" of "+db, output.ExitPrecondition,
				"List the dumps with eru dump list "+db+".").WithDetails(map[string]any{"reason": "dump_not_found", "database": db, "id": id})
		}
		return nil, s3Error(t, "read "+key, err)
	}
	obj, err := c.GetObject(ctx, t.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s3Error(t, "read "+key, err)
	}
	plain, err := age.Decrypt(obj, ident)
	if err != nil {
		_ = obj.Close()
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, output.NewError(output.CodeSecrets, "the recovery kit's age identity does not open dump "+id, output.ExitSecrets,
				"The dump was encrypted to another identity; use the recovery kit that was current when it was taken.")
		}
		return nil, s3Error(t, "read "+key, err)
	}
	return readCloser{Reader: plain, Closer: obj}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// DownloadDump writes the decrypted dump to out (0600), refusing to replace
// an existing file.
func DownloadDump(ctx context.Context, t Target, db, id, identity, out string) (int64, error) {
	r, err := OpenDump(ctx, t, db, id, identity)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator chose the output path.
	if err != nil {
		return 0, output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Pick a path that does not exist yet.")
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(out)
		return n, err
	}
	return n, nil
}

// DumpFileName is the default local file name for a dump.
func DumpFileName(db, id string) string {
	return path.Base(db + "-" + id + ".dump")
}

// connArgs are the -h and -p arguments that point a client tool at l.
func connArgs(l pg.Local) []string {
	var args []string
	if l.Host != "" {
		args = append(args, "-h", l.Host)
	}
	if l.Port != 0 {
		args = append(args, "-p", fmt.Sprint(l.Port))
	}
	return args
}
