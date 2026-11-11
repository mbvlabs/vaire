package activity

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/secrets"
)

func TestPrivacyCanariesAreRejectedAndNeverPersisted(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	canaries := []string{
		"SELECT * FROM pg_shadow WHERE usename='postgres'",
		"https://user:pass@example.com/hook",
		"line1\nline2",
		"password=hunter2",
		"-----BEGIN PRIVATE KEY-----",
		"\x1b[31mforged",
		"backup.full;drop",
	}
	for _, canary := range canaries {
		events := []Event{
			{Source: SourceRPC, Action: canary, Outcome: OutcomeStarted},
			{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, Database: canary},
			{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, OperationID: canary},
			{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, ErrorCode: canary},
			{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, ActorKind: ActorSystem, ActorLabel: canary},
			{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, NodeVersion: canary},
		}
		for _, ev := range events {
			_, err := s.Append(context.Background(), ev)
			if !errors.Is(err, errInvalidEvent) {
				t.Fatalf("Append accepted canary %q: %v", canary, err)
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("append error echoed canary %q: %v", canary, err)
			}
		}
		if _, err := s.Query(context.Background(), Query{Action: canary}); !errors.Is(err, errInvalidQuery) {
			t.Fatalf("Query accepted canary filter %q: %v", canary, err)
		}
	}
	assertNoCanary(t, s.Dir, canaries...)
}

func TestRegisteredSecretRemovesField(t *testing.T) {
	old := redactor
	registry := secrets.NewRegistry()
	const secret = "s3cr3tvalue"
	registry.Add(secret)
	redactor = registry
	t.Cleanup(func() { redactor = old })

	s := testStore(t)
	assigned := mustAppend(t, s, Event{
		Source:     SourceRPC,
		Action:     "backup.full",
		Outcome:    OutcomeSucceeded,
		Server:     secret,
		ActorKind:  ActorSystem,
		ActorLabel: secret,
	})
	if assigned.Server != "" || assigned.ActorLabel != "" {
		t.Fatalf("registered secret field survived: %+v", assigned)
	}
	if _, err := s.Append(context.Background(), Event{
		Source:  SourceRPC,
		Action:  secret,
		Outcome: OutcomeStarted,
	}); !errors.Is(err, errInvalidEvent) {
		t.Fatalf("secret action error = %v, want errInvalidEvent", err)
	}
	if _, err := s.Append(context.Background(), Event{
		Source:  SourceRPC,
		Action:  "backup." + secret,
		Outcome: OutcomeStarted,
	}); !errors.Is(err, errInvalidEvent) {
		t.Fatalf("partially redacted action error = %v, want errInvalidEvent", err)
	}
	assertNoCanary(t, s.Dir, secret)
}

func assertNoCanary(t *testing.T, dir string, canaries ...string) {
	t.Helper()
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return
	}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G304: a file inside the test store.
		if err != nil {
			return err
		}
		for _, canary := range canaries {
			if bytes.Contains(raw, []byte(canary)) {
				t.Fatalf("%s contains canary %q", path, canary)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk store: %v", err)
	}
}
