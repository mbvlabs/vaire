package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Target is a bucket and the keys to reach it. The operator builds it from
// credentials.json, the node from cluster.toml and /etc/eru/runtime/backup.env.
type Target struct {
	Endpoint string
	Region   string
	Bucket   string
	Prefix   string
	URIStyle string
	KeyID    string
	Secret   string
}

// Preset is a provider's defaults for the interview (§13.1).
type Preset struct {
	Provider string
	URIStyle string
	Region   string
	// Endpoint is a template; <account_id> and <region> are filled in by the
	// operator.
	Endpoint string
}

// Presets are the provider presets, in interview order.
var Presets = []Preset{
	{Provider: "r2", URIStyle: "path", Region: "auto", Endpoint: "https://<account_id>.r2.cloudflarestorage.com"},
	{Provider: "s3", URIStyle: "host", Region: "", Endpoint: "https://s3.<region>.amazonaws.com"},
	{Provider: "b2", URIStyle: "host", Region: "", Endpoint: "https://s3.<region>.backblazeb2.com"},
	{Provider: "hetzner", URIStyle: "host", Region: "fsn1", Endpoint: "https://<region>.your-objectstorage.com"},
	{Provider: "minio", URIStyle: "path", Region: "us-east-1", Endpoint: ""},
	{Provider: "custom", URIStyle: "path", Region: "", Endpoint: ""},
}

// PresetFor returns the preset for provider.
func PresetFor(provider string) (Preset, bool) {
	for _, p := range Presets {
		if p.Provider == provider {
			return p, true
		}
	}
	return Preset{}, false
}

// FillEndpoint replaces <region> in an endpoint template.
func FillEndpoint(template, region string) string {
	return strings.ReplaceAll(template, "<region>", region)
}

// client returns a minio client for t.
func (t Target) client() (*minio.Client, error) {
	u, err := url.Parse(t.Endpoint)
	if err != nil || u.Host == "" {
		return nil, output.NewError(output.CodeValidation, "backup endpoint "+t.Endpoint+" is not a URL", output.ExitConfig, "")
	}
	lookup := minio.BucketLookupDNS
	if t.URIStyle == "path" {
		lookup = minio.BucketLookupPath
	}
	return minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(t.KeyID, t.Secret, ""),
		Secure:       u.Scheme != "http",
		Region:       t.Region,
		BucketLookup: lookup,
	})
}

// key joins the prefix and rest into an object key.
func (t Target) key(rest string) string {
	return strings.Trim(t.Prefix, "/") + "/" + strings.TrimLeft(rest, "/")
}

// ProbeKey is the object the probe writes and removes.
const ProbeKey = ".eru-probe"

// Probe checks the bucket from wherever eru runs, before anything reaches
// the node: it reads the bucket, then puts, gets and deletes
// <prefix>/.eru-probe. It refuses a prefix whose pgBackRest repository
// belongs to another stanza. The error codes name the cause:
// backup_auth_failed, bucket_not_found and endpoint_unreachable.
func Probe(ctx context.Context, t Target, stanza string) error {
	c, err := t.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	exists, err := c.BucketExists(ctx, t.Bucket)
	if err != nil {
		return s3Error(t, "read the bucket", err)
	}
	if !exists {
		return bucketMissing(t)
	}
	body := []byte("eru probe " + time.Now().UTC().Format(time.RFC3339) + "\n")
	key := t.key(ProbeKey)
	if _, err := c.PutObject(ctx, t.Bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{ContentType: "text/plain"}); err != nil {
		return s3Error(t, "write "+key, err)
	}
	obj, err := c.GetObject(ctx, t.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return s3Error(t, "read "+key, err)
	}
	got, err := io.ReadAll(io.LimitReader(obj, 4096))
	_ = obj.Close()
	if err != nil {
		return s3Error(t, "read "+key, err)
	}
	if !bytes.Equal(got, body) {
		return output.NewError(output.CodeBackupAuthFailed, "the bucket returned different bytes than eru wrote to "+key, output.ExitTransport,
			"Check that the endpoint is the bucket's S3 API and not a proxy or cache.").WithDetails(map[string]any{"key": key})
	}
	if err := c.RemoveObject(ctx, t.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return s3Error(t, "delete "+key, err)
	}
	stanzas, err := t.stanzas(ctx, c)
	if err != nil {
		return err
	}
	for _, other := range stanzas {
		if other != stanza {
			return output.NewError(output.CodePrecondition,
				"prefix "+t.Prefix+" already holds the pgBackRest repository of cluster "+other,
				output.ExitPrecondition,
				"Pick another prefix (the default is the cluster name). Two clusters must never share a repository.").
				WithDetails(map[string]any{"reason": "repo_in_use", "stanza": other, "prefix": t.Prefix})
		}
	}
	return nil
}

// stanzas lists the stanza directories under <prefix>/pgbackrest/backup/.
func (t Target) stanzas(ctx context.Context, c *minio.Client) ([]string, error) {
	prefix := t.key("pgbackrest/backup/")
	var out []string
	for obj := range c.ListObjects(ctx, t.Bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		if obj.Err != nil {
			return nil, s3Error(t, "list "+prefix, obj.Err)
		}
		name := strings.Trim(strings.TrimPrefix(obj.Key, prefix), "/")
		if name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// s3Error maps a minio error to eru's codes.
func s3Error(t Target, what string, err error) error {
	resp := minio.ToErrorResponse(err)
	details := map[string]any{"endpoint": t.Endpoint, "bucket": t.Bucket, "action": what}
	if resp.Code != "" {
		details["s3_code"] = resp.Code
	}
	switch resp.Code {
	case "NoSuchBucket":
		return bucketMissing(t)
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidToken", "ExpiredToken", "Unauthorized", "Forbidden":
		return output.NewError(output.CodeBackupAuthFailed, "the bucket refused the keys: "+resp.Code+" while trying to "+what, output.ExitTransport,
			"Check the access key ID and secret, and that the key may read, write, list and delete in "+t.Bucket+".").WithDetails(details)
	}
	var netErr net.Error
	var dnsErr *net.DNSError
	var urlErr *url.Error
	if errors.As(err, &dnsErr) || errors.As(err, &netErr) || errors.As(err, &urlErr) || resp.StatusCode == 0 {
		return output.WrapError(output.CodeEndpointUnreachable, errors.New("cannot reach "+t.Endpoint+" to "+what+": "+err.Error()), output.ExitTransport,
			"Check the endpoint URL and that this machine reaches it over HTTPS.").WithDetails(details)
	}
	return output.WrapError(output.CodeExternalCommand, errors.New("the bucket failed to "+what+": "+err.Error()), output.ExitExternal, "").WithDetails(details)
}

func bucketMissing(t Target) error {
	return output.NewError(output.CodeBucketNotFound, "bucket "+t.Bucket+" does not exist at "+t.Endpoint, output.ExitTransport,
		"Create the bucket first, or fix the bucket name and region.").WithDetails(map[string]any{"bucket": t.Bucket, "endpoint": t.Endpoint})
}

// Object is one listed object.
type Object struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"last_modified"`
}

// list returns the objects under <prefix>/<rest>, sorted by key.
func (t Target) list(ctx context.Context, rest string) ([]Object, error) {
	c, err := t.client()
	if err != nil {
		return nil, err
	}
	prefix := t.key(rest)
	var out []Object
	for obj := range c.ListObjects(ctx, t.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, s3Error(t, "list "+prefix, obj.Err)
		}
		out = append(out, Object{Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified.UTC().Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
