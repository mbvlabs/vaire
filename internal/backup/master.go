package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/minio/minio-go/v7"
)

// The master bundles live under <prefix>/master/ (Phase 5): each bundle as
// <name>.tar.age, its sidecar <name>.json written after it, and epoch.json.

// masterTimeout bounds one call about the kilobyte-sized master objects.
const masterTimeout = 60 * time.Second

// maxMasterObject bounds what a master object read may return.
const maxMasterObject = 64 << 20

// MasterKey is the object key of rest under <prefix>/master/.
func (t Target) MasterKey(rest string) string { return t.key(masterbackup.BucketDir + rest) }

// MasterBundle is one bundle in the bucket.
type MasterBundle struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Epoch     int64  `json:"epoch"`
	Revision  string `json:"revision"`
	Size      int64  `json:"size"`
	// Complete is true once the sidecar exists: the bundle went up whole.
	Complete bool `json:"complete"`
	// Orphan is a sidecar without its bundle.
	Orphan bool `json:"orphan,omitempty"`
}

// MasterList lists the bundles under <prefix>/master/, newest first.
func MasterList(ctx context.Context, t Target) ([]MasterBundle, error) {
	ctx, cancel := context.WithTimeout(ctx, masterTimeout)
	defer cancel()
	objs, err := t.list(ctx, masterbackup.BucketDir)
	if err != nil {
		return nil, err
	}
	byName := map[string]*MasterBundle{}
	for _, o := range objs {
		name, isBundle, ok := masterbackup.SplitKey(o.Key)
		if !ok {
			continue
		}
		b, seen := byName[name]
		if !seen {
			p, _ := masterbackup.ParseName(name)
			b = &MasterBundle{Name: name, CreatedAt: p.CreatedAt.Format(time.RFC3339), Epoch: p.Epoch, Revision: p.Revision, Orphan: true}
			byName[name] = b
		}
		if isBundle {
			b.Size, b.Orphan = o.Size, false
		} else {
			b.Complete = true
		}
	}
	out := make([]MasterBundle, 0, len(byName))
	for _, b := range byName {
		if b.Orphan {
			b.Complete = false
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// MasterPut uploads a bundle, then its sidecar: a bundle without a sidecar
// is incomplete and never restored.
func MasterPut(ctx context.Context, t Target, name string, ciphertext []byte, sidecar masterbackup.Sidecar) error {
	c, err := t.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, masterTimeout)
	defer cancel()
	key := t.MasterKey(name + masterbackup.BundleSuffix)
	if _, err := c.PutObject(ctx, t.Bucket, key, bytes.NewReader(ciphertext), int64(len(ciphertext)),
		minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true}); err != nil {
		return s3Error(t, "upload "+key, err)
	}
	raw, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return err
	}
	skey := t.MasterKey(name + masterbackup.SidecarSuffix)
	if _, err := c.PutObject(ctx, t.Bucket, skey, bytes.NewReader(raw), int64(len(raw)),
		minio.PutObjectOptions{ContentType: "application/json", DisableMultipart: true}); err != nil {
		return s3Error(t, "upload "+skey, err)
	}
	return nil
}

// errNoSuchKey marks an object that doesn't exist.
var errNoSuchKey = errors.New("no such key")

// IsNoSuchKey reports whether err is a missing object.
func IsNoSuchKey(err error) bool { return errors.Is(err, errNoSuchKey) }

// masterGet reads one object under master/.
func masterGet(ctx context.Context, t Target, rest string) ([]byte, error) {
	c, err := t.client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, masterTimeout)
	defer cancel()
	key := t.MasterKey(rest)
	obj, err := c.GetObject(ctx, t.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s3Error(t, "read "+key, err)
	}
	defer func() { _ = obj.Close() }()
	raw, err := io.ReadAll(io.LimitReader(obj, maxMasterObject))
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, errNoSuchKey
		}
		return nil, s3Error(t, "read "+key, err)
	}
	return raw, nil
}

// MasterGet downloads a bundle and its sidecar.
func MasterGet(ctx context.Context, t Target, name string) ([]byte, masterbackup.Sidecar, error) {
	var side masterbackup.Sidecar
	raw, err := masterGet(ctx, t, name+masterbackup.SidecarSuffix)
	if err != nil {
		return nil, side, err
	}
	if err := json.Unmarshal(raw, &side); err != nil {
		return nil, side, errors.New("the sidecar of " + name + " does not parse: " + err.Error())
	}
	bundle, err := masterGet(ctx, t, name+masterbackup.BundleSuffix)
	if err != nil {
		return nil, side, err
	}
	return bundle, side, nil
}

// MasterHasSidecar reports whether a bundle's sidecar is in the bucket.
func MasterHasSidecar(ctx context.Context, t Target, name string) (bool, error) {
	c, err := t.client()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, masterTimeout)
	defer cancel()
	key := t.MasterKey(name + masterbackup.SidecarSuffix)
	if _, err := c.StatObject(ctx, t.Bucket, key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return false, nil
		}
		return false, s3Error(t, "read "+key, err)
	}
	return true, nil
}

// MasterDelete deletes a bundle's sidecar, then the bundle.
func MasterDelete(ctx context.Context, t Target, name string) error {
	c, err := t.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, masterTimeout)
	defer cancel()
	for _, suffix := range []string{masterbackup.SidecarSuffix, masterbackup.BundleSuffix} {
		key := t.MasterKey(name + suffix)
		if err := c.RemoveObject(ctx, t.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
			return s3Error(t, "delete "+key, err)
		}
	}
	return nil
}

// ReadEpoch reads <prefix>/master/epoch.json; nil when it doesn't exist.
func ReadEpoch(ctx context.Context, t Target) (*masterbackup.Epoch, error) {
	raw, err := masterGet(ctx, t, masterbackup.EpochObject)
	if IsNoSuchKey(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e masterbackup.Epoch
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, errors.New("epoch.json does not parse: " + err.Error())
	}
	return &e, nil
}

// WriteEpoch writes <prefix>/master/epoch.json.
func WriteEpoch(ctx context.Context, t Target, e masterbackup.Epoch) error {
	c, err := t.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, masterTimeout)
	defer cancel()
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	key := t.MasterKey(masterbackup.EpochObject)
	if _, err := c.PutObject(ctx, t.Bucket, key, bytes.NewReader(raw), int64(len(raw)),
		minio.PutObjectOptions{ContentType: "application/json", DisableMultipart: true}); err != nil {
		return s3Error(t, "write "+key, err)
	}
	return nil
}

// EpochModified is when the bucket stored epoch.json, by the bucket's own
// clock: the same clock as the archive objects NewestArchive compares it
// with, so the operator's clock (which wrote written_at) can't skew the
// comparison.
func EpochModified(ctx context.Context, t Target) (time.Time, error) {
	c, err := t.client()
	if err != nil {
		return time.Time{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, masterTimeout)
	defer cancel()
	key := t.MasterKey(masterbackup.EpochObject)
	info, err := c.StatObject(ctx, t.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return time.Time{}, s3Error(t, "read "+key, err)
	}
	return info.LastModified, nil
}

// NewestArchive returns when the newest object under the stanza's WAL
// archive was written, zero when there is none: db recover's check that an
// old master isn't still pushing WAL (D8).
func NewestArchive(ctx context.Context, t Target, stanza string) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	objs, err := t.list(ctx, "pgbackrest/archive/"+stanza+"/")
	if err != nil {
		return "", time.Time{}, err
	}
	var key string
	var newest time.Time
	for _, o := range objs {
		if strings.HasPrefix(path.Base(o.Key), "archive.info") {
			// The stanza's metadata, not WAL a master pushed.
			continue
		}
		at, err := time.Parse(time.RFC3339, o.LastModified)
		if err == nil && at.After(newest) {
			key, newest = o.Key, at
		}
	}
	return key, newest, nil
}
