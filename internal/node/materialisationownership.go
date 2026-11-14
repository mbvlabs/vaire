package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"golang.org/x/sys/unix"
)

// These records are node-local evidence, not an AwaitingRecovery record.
// They are deliberately not part of a master backup.
//
// Artifact binding is not an authorization boundary. Every action also
// requires the active-job capability supplied by the runner.
type materialisationOwnership struct {
	Operation           string                     `json:"operation"`
	Job                 string                     `json:"job"`
	Instance            string                     `json:"instance"`
	Kind                string                     `json:"kind"`
	Target              string                     `json:"target"`
	Source              string                     `json:"source"`
	Stanza              string                     `json:"stanza"`
	IntendedPort        int                        `json:"intended_port"`
	TemporaryPort       int                        `json:"temporary_port"`
	DataPath            string                     `json:"data_path"`
	RegisteredPath      string                     `json:"registered_path"`
	Phase               string                     `json:"phase"`
	Reason              string                     `json:"reason,omitempty"`
	ArtifactStage       string                     `json:"artifact_stage,omitempty"`
	DataIdentity        materialisationFileID      `json:"data_identity"`
	ConfigIdentity      materialisationFileID      `json:"config_identity"`
	ReservationIdentity backup.ReservationIdentity `json:"reservation_identity,omitempty"`
}

type materialisationFileID struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint32 `json:"uid"`
}

const (
	materialisationFork         = "fork"
	materialisationRestore      = "restore"
	materialisationRecover      = "recover"
	materialisationDrill        = "drill"
	materialisationFollower     = "follower"
	materialisationPrepared     = "prepared"
	materialisationCopying      = "copying"
	materialisationProvisioning = "provisioning"
	materialisationFailed       = "failed"
	materialisationDataCreated  = "data_created"
	materialisationRegistered   = "registered"
	materialisationProvisioned  = "provisioned"
)

var materialisationStateDir = filepath.Dir(backup.RecordsPath)
var materialisationRecordUID uint32
var materialisationConfigRoot = "/etc/postgresql/18"
var materialisationAncestorBase = "/"
var materialisationPostgresUID = func() (uint32, error) {
	uid, _, err := engine.ResolveIDs("postgres", "")
	if err != nil || uid < 0 {
		return 0, errors.New("cannot resolve postgres user")
	}
	return uint32(uid), nil //nolint:gosec // resolved operating system user id
}

func materialisationPath(target string) (string, error) {
	// Restore/drill names are generated from public database names and can
	// exceed PostgreSQL's identifier limit. Their exact relationship to the
	// source is checked below; this check only prevents unsafe path components.
	if target == "" || len(target) > 80 || target == "." || target == ".." ||
		strings.ContainsAny(target, `/\`) || strings.IndexByte(target, 0) >= 0 {
		return "", errors.New("invalid materialisation target")
	}
	return filepath.Join(materialisationStateDir, "materialise-"+target+".json"), nil
}

func validateMaterialisation(o materialisationOwnership, dataRoot string) error {
	if config.CheckDatabaseName(o.Source) != nil {
		return errors.New("invalid materialisation source")
	}
	if _, err := materialisationPath(o.Target); err != nil {
		return err
	}
	switch o.Kind {
	case materialisationFork, materialisationFollower:
		if config.CheckDatabaseName(o.Target) != nil {
			return errors.New("invalid materialisation target")
		}
	case materialisationRestore, materialisationRecover:
		if o.Target != backup.RestoringName(o.Source) {
			return errors.New("invalid restoring target")
		}
	case materialisationDrill:
		if o.Target != o.Source+"__drill" {
			return errors.New("invalid drill target")
		}
	default:
		return errors.New("invalid materialisation kind")
	}
	if o.Operation == "" || o.Job == "" || o.Instance == "" ||
		strings.TrimSpace(o.Operation) != o.Operation || strings.TrimSpace(o.Job) != o.Job {
		return errors.New("missing materialisation identity")
	}
	if o.Stanza == "" && o.Kind != materialisationFollower && o.Kind != materialisationFork {
		return errors.New("missing materialisation stanza")
	}
	if o.IntendedPort < 1 || o.IntendedPort > 65535 ||
		(o.TemporaryPort == 0 && o.ArtifactStage != "") ||
		(o.TemporaryPort != 0 && (o.TemporaryPort < config.TemporaryPortMin || o.TemporaryPort > config.TemporaryPortMax)) {
		return errors.New("invalid materialisation port")
	}
	if !filepath.IsAbs(dataRoot) || filepath.Clean(dataRoot) != dataRoot ||
		o.DataPath != filepath.Join(dataRoot, o.Target) ||
		o.RegisteredPath != filepath.Join(materialisationConfigRoot, o.Target) {
		return errors.New("invalid materialisation paths")
	}
	switch o.ArtifactStage {
	case "":
		if o.DataIdentity != (materialisationFileID{}) || o.ConfigIdentity != (materialisationFileID{}) {
			return errors.New("unexpected materialisation artifact identity")
		}
	case materialisationDataCreated:
		postgresUID, err := materialisationPostgresUID()
		if err != nil || o.DataIdentity.Inode == 0 || o.DataIdentity.UID != postgresUID ||
			o.ConfigIdentity != (materialisationFileID{}) {
			return errors.New("invalid data artifact identity")
		}
	case materialisationRegistered, materialisationProvisioned:
		postgresUID, err := materialisationPostgresUID()
		if err != nil || o.DataIdentity.Inode == 0 || o.ConfigIdentity.Inode == 0 ||
			o.DataIdentity.UID != postgresUID || o.ConfigIdentity.UID != postgresUID {
			return errors.New("invalid registered artifact identities")
		}
	default:
		return errors.New("invalid materialisation artifact stage")
	}
	switch o.Phase {
	case materialisationPrepared, materialisationCopying, materialisationProvisioning:
		if o.Reason != "" {
			return errors.New("unexpected materialisation failure reason")
		}
	case materialisationFailed:
		if o.Reason != "recovery_requirements_unmet" && o.Reason != "recovery_parameter_mismatch" {
			return errors.New("invalid materialisation failure reason")
		}
	default:
		return errors.New("invalid materialisation phase")
	}
	instance, err := currentFollowInstance()
	if err != nil {
		return err
	}
	if o.Instance != instance {
		return errors.New("foreign materialisation instance")
	}
	return nil
}

func readMaterialisation(target, dataRoot string) (materialisationOwnership, bool, error) {
	var o materialisationOwnership
	ancestors, err := materialisationAncestry(materialisationStateDir, materialisationRecordUID, false)
	if err != nil {
		return o, false, err
	}
	p, err := materialisationPath(target)
	if err != nil {
		return o, false, err
	}
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	f := os.NewFile(uintptr(fd), p)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return o, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != materialisationRecordUID || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 ||
		stat.Nlink != 1 || info.Size() > 16384 {
		return o, false, errors.New("unsafe materialisation record")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return o, false, err
	}
	after, err := os.Lstat(p)
	if err != nil || !os.SameFile(info, after) || len(raw) > 16384 || validateMaterialisationAncestry(ancestors) != nil {
		return o, false, errors.New("materialisation record changed while reading")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&o); err != nil {
		return o, false, fmt.Errorf("corrupt materialisation record: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return o, false, errors.New("trailing materialisation record data")
	}
	if o.Target != target {
		return o, false, errors.New("materialisation target mismatch")
	}
	if err := validateMaterialisation(o, dataRoot); err != nil {
		return o, false, err
	}
	return o, true, nil
}

func listMaterialisations(dataRoot string) ([]materialisationOwnership, error) {
	entries, err := os.ReadDir(materialisationStateDir)
	if err != nil {
		return nil, err
	}
	var result []materialisationOwnership
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "materialise-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		target := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "materialise-"), ".json")
		o, found, err := readMaterialisation(target, dataRoot)
		if err != nil || !found {
			return nil, fmt.Errorf("cannot validate materialisation record %q: %w", e.Name(), err)
		}
		result = append(result, o)
	}
	return result, nil
}

// A capability is an unexported, process-local object. Creating one does not
// establish that a job is active: the job runner must issue it only while that
// exact job holds its locks, and must not pass it to public apply.
type materialisationCapability struct{ owner materialisationOwnership }
type materialisationContextKey struct{}

func materialisationContext(ctx context.Context, capability *materialisationCapability) context.Context {
	return context.WithValue(ctx, materialisationContextKey{}, capability)
}

func ownsMaterialisation(ctx context.Context, o materialisationOwnership) bool {
	// No runner-issued lock-bound capability exists yet. Never treat a
	// matching in-memory value as authority.
	_ = ctx
	_ = o
	return false
}

// createMaterialisation refuses to replace any existing record.
func createMaterialisation(o materialisationOwnership, dataRoot string) (*materialisationCapability, error) {
	if o.Phase != materialisationPrepared || o.ArtifactStage != "" || o.TemporaryPort != 0 {
		return nil, errors.New("materialisation must start prepared")
	}
	if err := validateMaterialisation(o, dataRoot); err != nil {
		return nil, err
	}
	if os.Geteuid() < 0 || uint64(os.Geteuid()) != uint64(materialisationRecordUID) { //nolint:gosec // negative uid is rejected
		return nil, errors.New("materialisation record requires the trusted owner")
	}
	ancestors, err := materialisationAncestry(materialisationStateDir, materialisationRecordUID, false)
	if err != nil {
		return nil, err
	}
	p, _ := materialisationPath(o.Target)
	// Link the complete, fsynced file into place without replacing evidence of
	// another operation. The caller also holds the node operation lock.
	f, err := os.CreateTemp(materialisationStateDir, ".materialise-create-")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	raw, err := json.Marshal(o)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err := validateMaterialisationAncestry(ancestors); err != nil {
		return nil, err
	}
	if err := os.Link(tmp, p); err != nil {
		return nil, err
	}
	dir, err := os.Open(materialisationStateDir) //nolint:gosec // fixed node state directory
	if err != nil {
		return nil, err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return nil, err
	}
	// The caller must not receive an authority merely for writing a record.
	return nil, nil
}

// materialisationArtifacts keeps the two parent directories and created
// artifacts pinned for the duration of an active job. It is not an active-job
// capability; the job lease must independently authorize every call.
type materialisationArtifacts struct {
	owner                            materialisationOwnership
	dataRoot                         string
	dataUID                          uint32
	dataParent, configParent         *os.Root
	dataParentInfo, configParentInfo os.FileInfo
	dataAncestors, configAncestors   []materialisationAncestor
	data, conf                       *os.File
	recordInfo                       os.FileInfo
}

func materialisationID(info os.FileInfo) (materialisationFileID, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || st.Ino == 0 {
		return materialisationFileID{}, errors.New("unverifiable artifact directory")
	}
	return materialisationFileID{Device: uint64(st.Dev), Inode: st.Ino, UID: st.Uid}, nil
}

// Separate seams let rootless tests model the postgres-owned artifacts without
// changing the real filesystem's ownership.
var materialisationArtifactID = materialisationID
var materialisationConfigureArtifact = func(f *os.File, uid uint32, mode os.FileMode) error {
	if err := f.Chown(int(uid), -1); err != nil {
		return err
	}
	return f.Chmod(mode)
}

type materialisationAncestor struct {
	path string
	info os.FileInfo
	id   materialisationFileID
}

// Tests can model distinct service and root ownership without chowning files.
var materialisationAncestryStat = func(path string) (os.FileInfo, materialisationFileID, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, materialisationFileID{}, err
	}
	id, err := materialisationID(info)
	return info, id, err
}

// PostgreSQL is a trusted service identity and all vaire operations obey the
// exclusive target lock. This does not defend against a hostile postgres/root
// process racing mkdir with open; Unix mkdir does not return a directory fd.
func materialisationAncestry(path string, serviceUID uint32, allowService bool) ([]materialisationAncestor, error) {
	base := materialisationAncestorBase
	rel, err := filepath.Rel(base, path)
	if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("invalid materialisation parent path")
	}
	paths := []string{base}
	current := base
	if rel != "." {
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			paths = append(paths, current)
		}
	}
	ancestors := make([]materialisationAncestor, 0, len(paths))
	for i, p := range paths {
		info, id, err := materialisationAncestryStat(p)
		if err != nil {
			return nil, err
		}
		// Only data paths admit the resolved service owner, and never at the
		// traversal root itself. Registration and state remain root-only.
		allowed := id.UID == materialisationRecordUID || allowService && i > 0 && id.UID == serviceUID
		if !allowed || info.Mode().Perm()&0o022 != 0 {
			return nil, errors.New("unsafe materialisation ancestry")
		}
		ancestors = append(ancestors, materialisationAncestor{p, info, id})
	}
	return ancestors, nil
}

func validateMaterialisationAncestry(ancestors []materialisationAncestor) error {
	for _, a := range ancestors {
		info, id, err := materialisationAncestryStat(a.path)
		if err != nil || !info.IsDir() || !os.SameFile(a.info, info) || info.Mode().Perm()&0o022 != 0 {
			return errors.New("materialisation ancestry replaced or unsafe")
		}
		if a.id != id {
			return errors.New("materialisation ancestry ownership changed")
		}
	}
	return nil
}

func openMaterialisationParent(path string, serviceUID uint32, allowService bool) (*os.Root, os.FileInfo, []materialisationAncestor, error) {
	ancestors, err := materialisationAncestry(path, serviceUID, allowService)
	if err != nil {
		return nil, nil, nil, err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return nil, nil, nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, nil, err
	}
	f, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, nil, err
	}
	opened, err := f.Stat()
	_ = f.Close()
	if err != nil || !os.SameFile(named, opened) || validateMaterialisationAncestry(ancestors) != nil {
		_ = root.Close()
		return nil, nil, nil, errors.New("materialisation parent was replaced")
	}
	return root, opened, ancestors, nil
}

// openMaterialisationArtifacts reads a trusted record and pins both parents.
// It never adopts an unbound existing directory.
func openMaterialisationArtifacts(target, dataRoot string, dataUID uint32) (*materialisationArtifacts, error) {
	postgresUID, err := materialisationPostgresUID()
	if err != nil || dataUID != postgresUID {
		return nil, errors.New("untrusted materialisation data owner")
	}
	o, found, err := readMaterialisation(target, dataRoot)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("materialisation record missing")
	}
	if o.DataIdentity.Inode != 0 && o.DataIdentity.UID != dataUID {
		return nil, errors.New("materialisation data owner mismatch")
	}
	p, _ := materialisationPath(target)
	recordInfo, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	a := &materialisationArtifacts{owner: o, dataRoot: dataRoot, dataUID: dataUID, recordInfo: recordInfo}
	a.dataParent, a.dataParentInfo, a.dataAncestors, err = openMaterialisationParent(dataRoot, dataUID, true)
	if err != nil {
		return nil, err
	}
	a.configParent, a.configParentInfo, a.configAncestors, err = openMaterialisationParent(materialisationConfigRoot, materialisationRecordUID, false)
	if err != nil {
		_ = a.dataParent.Close()
		return nil, err
	}
	if err := a.validate(); err != nil {
		a.close()
		return nil, err
	}
	if o.DataIdentity.Inode != 0 {
		a.data, err = openMaterialisationChild(a.dataParent, target)
		if err != nil {
			a.close()
			return nil, err
		}
	}
	if o.ConfigIdentity.Inode != 0 {
		a.conf, err = openMaterialisationChild(a.configParent, target)
		if err != nil {
			a.close()
			return nil, err
		}
	}
	if err := a.validate(); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

func (a *materialisationArtifacts) close() {
	if a.data != nil {
		_ = a.data.Close()
	}
	if a.conf != nil {
		_ = a.conf.Close()
	}
	if a.dataParent != nil {
		_ = a.dataParent.Close()
	}
	if a.configParent != nil {
		_ = a.configParent.Close()
	}
}

func (a *materialisationArtifacts) validateRecord() error {
	p, _ := materialisationPath(a.owner.Target)
	before, err := os.Lstat(p)
	if err != nil || !os.SameFile(a.recordInfo, before) {
		return errors.New("materialisation record replaced")
	}
	current, found, err := readMaterialisation(a.owner.Target, a.dataRoot)
	if err != nil {
		return err
	}
	after, err := os.Lstat(p)
	if err != nil || !found || current != a.owner || !os.SameFile(a.recordInfo, after) {
		return errors.New("materialisation record changed")
	}
	return nil
}

func validateMaterialisationParent(path string, expected os.FileInfo) error {
	info, err := os.Lstat(path)
	if err != nil || !os.SameFile(expected, info) || !info.IsDir() {
		return errors.New("materialisation parent replaced")
	}
	return nil
}

func openMaterialisationChild(root *os.Root, name string) (*os.File, error) {
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func validateMaterialisationDir(root *os.Root, name string, id materialisationFileID, held *os.File, mode os.FileMode) error {
	named, err := root.Lstat(name)
	if id == (materialisationFileID{}) {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("unbound materialisation artifact exists")
	}
	if err != nil {
		return err
	}
	actual, err := materialisationArtifactID(named)
	if err != nil || actual != id || named.Mode().Perm() != mode {
		return errors.New("materialisation artifact replaced")
	}
	f, err := openMaterialisationChild(root, name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(named, opened) || opened.Mode().Perm() != mode {
		return errors.New("materialisation artifact changed while opening")
	}
	if held != nil {
		old, err := held.Stat()
		if err != nil || !os.SameFile(old, opened) {
			return errors.New("materialisation artifact handle changed")
		}
	}
	return nil
}

func (a *materialisationArtifacts) validate() error {
	if err := a.validateRecord(); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.dataAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.configAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationParent(a.dataRoot, a.dataParentInfo); err != nil {
		return err
	}
	if err := validateMaterialisationParent(materialisationConfigRoot, a.configParentInfo); err != nil {
		return err
	}
	if err := validateMaterialisationDir(a.dataParent, a.owner.Target, a.owner.DataIdentity, a.data, 0o700); err != nil {
		return err
	}
	return validateMaterialisationDir(a.configParent, a.owner.Target, a.owner.ConfigIdentity, a.conf, 0o755)
}

// bindCreated creates one directory exclusively and persists its identity
// before returning it to a tool. Existing or crash-left directories are refused.
func (a *materialisationArtifacts) bindCreated(configDir bool) error {
	if err := a.validate(); err != nil {
		return err
	}
	if a.owner.TemporaryPort == 0 {
		return errors.New("temporary port not reserved")
	}
	root, uid, mode := a.dataParent, a.dataUID, os.FileMode(0o700)
	if configDir {
		if a.owner.ArtifactStage != materialisationDataCreated {
			return errors.New("data must be bound first")
		}
		root, mode = a.configParent, 0o755
	} else if a.owner.ArtifactStage != "" {
		return errors.New("data already bound")
	}
	if err := root.Mkdir(a.owner.Target, 0o700); err != nil {
		return err
	}
	f, err := openMaterialisationChild(root, a.owner.Target)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	named, err := root.Lstat(a.owner.Target)
	if err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(named, opened) {
		return errors.New("created artifact replaced")
	}
	id, err := materialisationArtifactID(opened)
	if err != nil || id.UID != materialisationRecordUID || opened.Mode().Perm() != 0o700 {
		return errors.New("created artifact is not a private directory owned by the creator")
	}
	if err := materialisationConfigureArtifact(f, uid, mode); err != nil {
		return err
	}
	opened, err = f.Stat()
	if err != nil {
		return err
	}
	id, err = materialisationArtifactID(opened)
	named, namedErr := root.Lstat(a.owner.Target)
	if err != nil || namedErr != nil || !os.SameFile(named, opened) || id.UID != uid || opened.Mode().Perm() != mode {
		return errors.New("created artifact owner mismatch")
	}
	if err := a.validateRecord(); err != nil {
		return err
	}
	if err := validateMaterialisationParent(a.dataRoot, a.dataParentInfo); err != nil {
		return err
	}
	if err := validateMaterialisationParent(materialisationConfigRoot, a.configParentInfo); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.dataAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.configAncestors); err != nil {
		return err
	}
	if configDir {
		if err := validateMaterialisationDir(a.dataParent, a.owner.Target, a.owner.DataIdentity, a.data, 0o700); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	parent, err := root.Open(".")
	if err != nil {
		return err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return err
	}
	next := a.owner
	if configDir {
		next.ConfigIdentity, next.ArtifactStage = id, materialisationRegistered
	} else {
		next.DataIdentity, next.ArtifactStage = id, materialisationDataCreated
	}
	if err := validateMaterialisation(next, a.dataRoot); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	p, _ := materialisationPath(next.Target)
	if err := engine.WriteFileAtomic(p, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	a.recordInfo, a.owner = info, next
	if configDir {
		a.conf = f
	} else {
		a.data = f
	}
	f = nil
	return a.validate()
}

// withOwnedArtifacts only checks artifacts, not job authority. Its caller must
// also possess the active owning job's lock-bound capability.
func (a *materialisationArtifacts) withOwnedArtifacts(fn func() error) error {
	if a.owner.ArtifactStage != materialisationRegistered && a.owner.ArtifactStage != materialisationProvisioned {
		return errors.New("materialisation artifacts are not registered")
	}
	if err := a.validate(); err != nil {
		return err
	}
	err := fn()
	return errors.Join(err, a.validate())
}

// markMaterialisationProvisioned is called only after the trusted lifecycle
// verifier has established completion, under the active job's lease.
func (a *materialisationArtifacts) markMaterialisationProvisioned(verify func() error) error {
	if a.owner.ArtifactStage != materialisationRegistered || verify == nil {
		return errors.New("materialisation is not ready for provisioning verification")
	}
	if err := a.withOwnedArtifacts(verify); err != nil {
		return err
	}
	next := a.owner
	next.ArtifactStage = materialisationProvisioned
	return a.persistMaterialisation(next)
}

func (a *materialisationArtifacts) persistMaterialisation(next materialisationOwnership) error {
	if err := a.validate(); err != nil {
		return err
	}
	current := a.owner
	// Only phase, failure reason, the initial reservation, and the verified
	// registered-to-provisioned stage may change here. Artifact creation has
	// its own exclusive-create-and-bind path; an existing identity is never
	// refreshed from a named directory.
	allowed := current
	allowed.Phase, allowed.Reason = next.Phase, next.Reason
	allowed.TemporaryPort, allowed.ArtifactStage = next.TemporaryPort, next.ArtifactStage
	allowed.ReservationIdentity = next.ReservationIdentity
	if allowed != next {
		return errors.New("materialisation identity cannot change")
	}
	portChanged := next.TemporaryPort != current.TemporaryPort
	reservationChanged := next.ReservationIdentity != current.ReservationIdentity
	stageChanged := next.ArtifactStage != current.ArtifactStage
	phaseChanged := next.Phase != current.Phase || next.Reason != current.Reason
	if portChanged && (current.TemporaryPort != 0 || current.ArtifactStage != "" ||
		next.TemporaryPort < config.TemporaryPortMin || next.TemporaryPort > config.TemporaryPortMax || stageChanged || phaseChanged) {
		return errors.New("invalid materialisation reservation transition")
	}
	if reservationChanged && (!portChanged || current.ReservationIdentity != (backup.ReservationIdentity{})) {
		return errors.New("invalid materialisation reservation identity transition")
	}
	if stageChanged && (current.ArtifactStage != materialisationRegistered ||
		next.ArtifactStage != materialisationProvisioned || portChanged || phaseChanged) {
		return errors.New("invalid materialisation artifact transition")
	}
	if phaseChanged {
		allowedPhase := current.Phase == materialisationPrepared && next.Phase == materialisationCopying ||
			current.Phase == materialisationCopying && next.Phase == materialisationProvisioning ||
			(current.Phase == materialisationPrepared || current.Phase == materialisationCopying || current.Phase == materialisationProvisioning) &&
				next.Phase == materialisationFailed
		if !allowedPhase || portChanged || stageChanged {
			return errors.New("invalid materialisation phase transition")
		}
	}
	if err := validateMaterialisation(next, a.dataRoot); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	p, _ := materialisationPath(current.Target)
	// The caller holds the exclusive operation lock and the state directory is
	// root-controlled. Recheck the precise original record immediately before
	// its atomic replacement.
	if err := a.validateRecord(); err != nil {
		return err
	}
	if err := engine.WriteFileAtomic(p, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	a.recordInfo, a.owner = info, next
	return a.validate()
}

// The trusted allocator adapter acquires its lock, verifies the exact
// reservation was released (or never allocated), then returns a non-failing
// unlock function. The proof must remain valid until unlock. It is not a
// public flag or a boolean assertion.
type materialisationReservationGuard func(materialisationOwnership) (unlock func(), err error)

var materialisationRemoveRecord = os.Remove
var materialisationSyncArtifactParent = func(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// bindMaterialisationReservation requires a trusted allocator adapter to
// verify the exact target/port reservation under its lock while finish runs.
// It cannot be used to replace an already-bound reservation.
func (a *materialisationArtifacts) bindMaterialisationReservation(port int, verify func(materialisationOwnership, int, func() error) error, identity ...backup.ReservationIdentity) error {
	if verify == nil || a.owner.ArtifactStage != "" || a.owner.TemporaryPort != 0 {
		return errors.New("materialisation reservation cannot be bound")
	}
	called := false
	err := verify(a.owner, port, func() error {
		if called {
			return errors.New("reservation verifier called twice")
		}
		called = true
		next := a.owner
		next.TemporaryPort = port
		if len(identity) != 0 {
			next.ReservationIdentity = identity[0]
		}
		return a.persistMaterialisation(next)
	})
	if err != nil {
		return err
	}
	if !called {
		return errors.New("reservation was not verified")
	}
	return nil
}

func (a *materialisationArtifacts) retireMaterialisation(guard materialisationReservationGuard, check func() error) error {
	if guard == nil || check == nil {
		return errors.New("missing reservation or retirement verification")
	}
	if err := check(); err != nil {
		return err
	}
	unlock, err := guard(a.owner)
	if err != nil {
		return err
	}
	if unlock == nil {
		return errors.New("reservation guard did not acquire lock")
	}
	defer unlock()
	if err := check(); err != nil {
		return err
	}
	return a.removeMaterialisationRecord()
}

// Unlink is the logical commit point and the final fallible operation. Its
// absence is not fsynced: after a crash the marker may reappear, in which case
// normal ownership guards conservatively block it rather than adopting it.
func (a *materialisationArtifacts) removeMaterialisationRecord() error {
	if err := a.validateRecord(); err != nil {
		return err
	}
	p, _ := materialisationPath(a.owner.Target)
	return materialisationRemoveRecord(p)
}

func (a *materialisationArtifacts) retireProvisioned(guard materialisationReservationGuard) error {
	if a.owner.ArtifactStage != materialisationProvisioned {
		return errors.New("materialisation not provisioned")
	}
	return a.retireMaterialisation(guard, a.validate)
}

func (a *materialisationArtifacts) validateCleanupCompleted() error {
	if err := a.validateRecord(); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.dataAncestors); err != nil {
		return err
	}
	if err := validateMaterialisationAncestry(a.configAncestors); err != nil {
		return err
	}
	for _, item := range []struct {
		root *os.Root
		held *os.File
		id   materialisationFileID
	}{
		{a.dataParent, a.data, a.owner.DataIdentity}, {a.configParent, a.conf, a.owner.ConfigIdentity},
	} {
		if _, err := item.root.Lstat(a.owner.Target); !errors.Is(err, os.ErrNotExist) {
			return errors.New("materialisation artifact still exists or is unverifiable")
		}
		if item.id.Inode != 0 {
			if item.held == nil {
				return errors.New("missing original artifact handle")
			}
			info, err := item.held.Stat()
			if err != nil {
				return err
			}
			id, err := materialisationArtifactID(info)
			st, ok := info.Sys().(*syscall.Stat_t)
			if err != nil || !ok || id != item.id || st.Nlink != 0 {
				return errors.New("original materialisation artifact was not removed")
			}
		}
	}
	return nil
}

// retireCleaned performs cleanup only after validating the exact bound objects,
// then requires their names absent and the pinned original inodes unlinked.
// A partial failure intentionally retains the record and cannot be adopted.
func (a *materialisationArtifacts) retireCleaned(cleanup func() error, guard materialisationReservationGuard) error {
	if cleanup == nil || guard == nil {
		return errors.New("missing owned cleanup")
	}
	if err := a.validate(); err != nil {
		return err
	}
	unlock, err := guard(a.owner)
	if err != nil {
		return err
	}
	if unlock == nil {
		return errors.New("reservation guard did not acquire lock")
	}
	defer unlock()
	if err := a.validate(); err != nil {
		return err
	}
	if err := cleanup(); err != nil {
		return err
	}
	if err := a.validateCleanupCompleted(); err != nil {
		return err
	}
	for _, root := range []*os.Root{a.dataParent, a.configParent} {
		if err := materialisationSyncArtifactParent(root); err != nil {
			return err
		}
	}
	if err := a.validateCleanupCompleted(); err != nil {
		return err
	}
	return a.removeMaterialisationRecord()
}

func advanceMaterialisation(c *materialisationCapability, dataRoot, phase, reason string) error {
	_ = c
	_ = dataRoot
	_ = phase
	_ = reason
	return errors.New("materialisation capability issuance is unavailable")
}
