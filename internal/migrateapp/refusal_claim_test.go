package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// A journal-free refusal must preserve the entire project, even when .agents
// predates this invocation. A pre-existing lock would hide leaked claim paths.
func TestMigrationRefusalPreservesClaimTree(t *testing.T) {
	for _, kind := range []string{"no-agents", "pin-conflict"} {
		for _, parent := range []string{"absent", "existing", "live-owner"} {
			for _, dry := range []bool{true, false} {
				for _, format := range []string{"text", "json", "service"} {
					t.Run(fmt.Sprintf("%s/%s/dry=%t/%s", kind, parent, dry, format), func(t *testing.T) {
						root := noAgentConsumer(t)
						code, message := "migrate_failed", "no supported agent output"
						options := Options{DryRun: dry, VendorUnmapped: true}
						args := []string{"migrate", "tessl", "--project", root, "--vendor-unmapped"}
						if kind == "pin-conflict" {
							root = seedConsumer(t)
							writeFile(t, root, "agents.yaml", []byte("schemaVersion: 2\nagents: [claude-code]\ndependencies:\n  - source: github:example/alpha\n    requested: v1.0.0\n"), 0o644)
							options = Options{DryRun: dry, CLIMappings: []migrate.Mapping{{From: "example/alpha", Source: "github:example/alpha", Requested: "latest"}}}
							args = []string{"migrate", "tessl", "--project", root, "--map", "example/alpha=github:example/alpha@latest"}
							code, message = cli.CodeProjectStateConflict, "--map 'example/alpha=github:example/alpha@v1.0.0'"
						}
						if parent != "absent" {
							if err := os.Mkdir(filepath.Join(root, ".agents"), 0o750); err != nil {
								t.Fatal(err)
							}
						}
						var owner os.FileInfo
						lockPath := filepath.Join(root, ".agents/.acr-transactions/.lock")
						if parent == "live-owner" {
							hostileHoldClaim(t, root)
							var err error
							owner, err = os.Lstat(lockPath)
							if err != nil {
								t.Fatal(err)
							}
							if !dry {
								code, message = cli.CodeTransactionBusy, "transaction_busy"
							}
						}
						writeFile(t, root, "caller-note.txt", []byte("Preserve caller state.\n"), 0o640)
						before := hashTreeWithModes(t, root)
						app := NewApplication(remigrationRemote(t), "test")
						if dry {
							args = append(args, "--dry-run")
						}
						if format == "json" {
							args = append(args, "--json")
						}
						for attempt := 1; attempt <= 2; attempt++ {
							if format == "service" {
								_, err := app.service.Migrate(context.Background(), root, options)
								var busy *realize.TransactionBusyError
								var refusal *Error
								if parent == "live-owner" && !dry {
									if !errors.As(err, &busy) {
										t.Fatalf("expected live-owner refusal, got %v", err)
									}
								} else if !errors.As(err, &refusal) || refusal.Code != code || !strings.Contains(refusal.Message, message) {
									t.Fatalf("wrong service refusal: %v", err)
								}
							} else {
								stdout, stderr, exit := runCLI(t, app, args...)
								if exit != cli.ExitOperational || stdout != "" || !strings.Contains(stderr, message) {
									t.Fatalf("wrong refusal: %d %s %s", exit, stdout, stderr)
								}
								if format == "json" {
									var envelope struct{ Error struct{ Code string } }
									if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
										t.Fatal(err)
									}
									if envelope.Error.Code != code {
										t.Fatalf("wrong code: %s", stderr)
									}
								}
							}
							after := hashTreeWithModes(t, root)
							if !mapsEqual(before, after) {
								for path, value := range after {
									if before[path] != value {
										t.Errorf("attempt %d changed %s: %q -> %q", attempt, path, before[path], value)
									}
								}
								for path := range before {
									if _, ok := after[path]; !ok {
										t.Errorf("attempt %d removed %s", attempt, path)
									}
								}
							}
							if owner != nil {
								current, err := os.Lstat(lockPath)
								if err != nil || !os.SameFile(owner, current) {
									t.Fatalf("live claim inode changed: %v", err)
								}
								assertRefusalClaimStillLocked(t, lockPath)
							}
						}
					})
				}
			}
		}
	}
}

func assertRefusalClaimStillLocked(t *testing.T, filename string) {
	t.Helper()
	contender, err := os.OpenFile(filename, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		if unlockErr := syscall.Flock(int(contender.Fd()), syscall.LOCK_UN); unlockErr != nil {
			t.Error(unlockErr)
		}
		t.Error("live claim no longer excludes another descriptor")
	} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Errorf("unexpected contention error: %v", err)
	}
	if err := contender.Close(); err != nil {
		t.Fatal(err)
	}
}
