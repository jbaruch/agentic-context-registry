package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexCredentialBoundaryCLI(t *testing.T) {
	binary := journeyBuiltBinary(t)
	for _, scenario := range []string{"rotated_refusal", "rotated_clean"} {
		t.Run(scenario, func(t *testing.T) {
			const seed = "synthetic-cli-seed-credential-0123456789"
			const rotated = "synthetic-cli-rotated-credential-0123456789"
			project := newJourneyProject(t, nil)
			root := cleanProducerFixture(t)
			name := "packages/compass/skills/answer/info.py"
			original := "print('tessl install old/compass')\n"
			reverify2Put(t, root, name, original, 0644)
			journeyGit(t, root, "add", "-A")
			journeyGit(t, root, "commit", "-qm", "Original fixture")
			head := journeyGit(t, root, "rev-parse", "HEAD")
			index := journeyGit(t, root, "ls-files", "--stage")
			content := "print('ACR compass')\n"
			if scenario == "rotated_refusal" {
				content += "# " + rotated + "\n"
			}
			sum := sha256.Sum256([]byte(original))
			proposed := map[string]any{"edits": []any{map[string]any{"path": name, "beforeDigest": "sha256:" + hex.EncodeToString(sum[:]), "action": "replace", "content": content, "replacements": []any{}}}, "policyChanges": []any{}}
			encoded, err := json.Marshal(proposed)
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			command := exec.Command("go", "build", "-ldflags", "-X main.proposed="+base64.StdEncoding.EncodeToString(encoded), "-o", filepath.Join(bin, "codex"), "./testdata/codex-boundary")
			command.Env = append(os.Environ(), "CGO_ENABLED=0")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("build fixture: %v\n%s", err, output)
			}
			home := t.TempDir()
			auth := `{"tokens":{"refresh_token":"` + seed + `"}}`
			reverify2Put(t, home, "auth.json", auth, 0600)
			t.Setenv("CODEX_HOME", home)
			t.Setenv("CODEX_API_KEY", "")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			args := []string{"migrate", "tessl-plugin", filepath.Join(root, "packages/compass"), "--acr-only", "--repository", "https://github.com/example/semantic-compass", "--agent", "codex", "--json"}
			before := snapshotProjectTree(t, root)
			for _, dry := range []bool{true, false} {
				argv := append([]string{}, args...)
				if dry {
					argv = append(argv, "--dry-run")
				}
				want := 0
				if scenario == "rotated_refusal" {
					want = 1
				}
				result := project.runBinary(binary, want, argv...)
				for _, secret := range []string{seed, rotated} {
					if strings.Contains(result.stdout+result.stderr, secret) {
						t.Fatal("credential escaped CLI")
					}
				}
				evidence := filepath.Join(t.TempDir(), "result.json")
				if err := os.WriteFile(evidence, []byte(result.stdout+result.stderr), 0600); err != nil {
					t.Fatal(err)
				}
				actual, err := os.ReadFile(evidence)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{seed, rotated} {
					if strings.Contains(string(actual), secret) {
						t.Fatal("credential in saved evidence")
					}
				}
				if dry || want != 0 {
					assertTreeUnchanged(t, before, root, "credential boundary refusal/preview")
				}
				if want == 0 {
					report := journeyResult(t, result.stdout)
					boundary, ok := report["credentialBoundary"].(map[string]any)
					if !ok || boundary["planChecked"] != true || boundary["reportSanitized"] != true || boundary["applicationChecked"] != !dry {
						t.Fatalf("missing checked CLI boundary: %#v", boundary)
					}
				}
				if journeyGit(t, root, "rev-parse", "HEAD") != head || journeyGit(t, root, "ls-files", "--stage") != index {
					t.Fatal("conversion changed Git HEAD/index")
				}
			}
			assertFileBody(t, home, "auth.json", auth)
			if entries, err := os.ReadDir(filepath.Join(project.stateHome, "codex")); err != nil || len(entries) != 0 {
				t.Fatalf("private home cleanup: %v", err)
			}
			if scenario == "rotated_clean" {
				project.runBinary(binary, 0, "validate", root, "--json")
				journeyGit(t, root, "add", "-A")
				journeyGit(t, root, "commit", "-qm", "Actual converted fixture")
				objects := journeyGit(t, root, "rev-list", "--objects", "--all")
				for _, line := range strings.Split(objects, "\n") {
					fields := strings.Fields(line)
					if len(fields) == 0 {
						continue
					}
					body := journeyGit(t, root, "cat-file", "-p", fields[0])
					for _, secret := range []string{seed, rotated} {
						if strings.Contains(body, secret) {
							t.Fatal("credential in reachable Git object")
						}
					}
				}
			}
		})
	}
}
