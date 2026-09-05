package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

// referencePackage is one Tessl plugin the migration converts and publishes.
// Both packages declare the same `skills/advocate` path on purpose: the only
// thing separating "my own installed tree" from "somebody else's" in a
// `.tessl/plugins/<identity>/skills/advocate/...` reference is the identity.
type referencePackage struct {
	identity string
	commit   string
	release  int64
}

var (
	ownedReferencePackage     = referencePackage{identity: "legacy-workspace/advocate-plugin", commit: strings.Repeat("a", 40), release: 501}
	unrelatedReferencePackage = referencePackage{identity: "other-workspace/other-plugin", commit: strings.Repeat("b", 40), release: 502}
)

func (pkg referencePackage) source() string { return "github:" + pkg.identity }

func (pkg referencePackage) archiveRoot() string {
	return strings.ReplaceAll(pkg.identity, "/", "-") + "-" + pkg.commit[:7]
}

func (pkg referencePackage) helper() string {
	return "#!/usr/bin/env bash\nset -euo pipefail\nprintf '%s\\n' \"{\\\"helper\\\":\\\"" + pkg.identity + "\\\",\\\"argument\\\":\\\"${1:-none}\\\"}\"\n"
}

// referenceSkillBody addresses this package's own helper through both
// supported forms and the other package's helper through the form ACR must
// never claim, so one realized file carries the whole R1 decision.
func referenceSkillBody(owner, other referencePackage) string {
	return "---\nname: advocate\ndescription: Fixture skill for the reference migration journey.\n---\n\n# Advocate\n\n" +
		"Run `skills/advocate/scripts/check.sh --root`.\n\n" +
		"Run `.tessl/plugins/" + owner.identity + "/skills/advocate/scripts/check.sh --legacy`.\n\n" +
		"Keep `.tessl/plugins/" + other.identity + "/skills/advocate/scripts/check.sh`.\n\n" +
		"Keep `https://example.com/?next=skills/advocate/scripts/check.sh`.\n"
}

func referenceRuleBody() string {
	return "---\nalwaysApply: true\n---\n\n# Measurement\n\nRun `skills/advocate/scripts/check.sh --rule` before reporting a measurement.\n"
}

// convertReferencePackage runs the production producer migration over a Tessl
// plugin and returns the agent-plugin.yaml it wrote. The published manifest is
// the migration's own output, never a literal this test typed, so a migration
// that stops recording the Tessl identity fails the journey.
func convertReferencePackage(t *testing.T, pkg referencePackage, skill, rule string) string {
	t.Helper()
	producer := t.TempDir()
	plugin := map[string]any{
		"name":        pkg.identity,
		"version":     "1.0.0",
		"description": "Reference migration journey fixture package.",
		"private":     false,
		"repository":  "https://github.com/" + pkg.identity,
		"rules":       []string{"rules/measurement.md"},
		"skills":      []string{"skills/advocate"},
	}
	encoded, err := json.Marshal(plugin)
	if err != nil {
		t.Fatal(err)
	}
	reverify2Put(t, producer, ".tessl-plugin/plugin.json", string(encoded), 0o644)
	reverify2Put(t, producer, "rules/measurement.md", rule, 0o644)
	reverify2Put(t, producer, "skills/advocate/SKILL.md", skill, 0o644)
	reverify2Put(t, producer, "skills/advocate/scripts/check.sh", pkg.helper(), 0o755)

	if _, err := tesslplugin.Convert(tesslplugin.Options{PackageRoot: producer, Repository: "https://github.com/" + pkg.identity}); err != nil {
		t.Fatalf("convert %s: %v", pkg.identity, err)
	}
	converted, err := os.ReadFile(filepath.Join(producer, "agent-plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(converted, []byte("tesslIdentity: "+pkg.identity)) {
		t.Fatalf("migration did not record the Tessl identity of %s:\n%s", pkg.identity, converted)
	}
	return string(converted)
}

func referenceArchive(t *testing.T, pkg referencePackage, manifest, skill, rule string) []byte {
	t.Helper()
	entries := []ffaTarEntry{{name: pkg.archiveRoot() + "/", kind: tar.TypeDir, mode: 0o755}}
	for _, file := range []ffaTarEntry{
		{name: "agent-plugin.yaml", body: manifest},
		{name: "rules/measurement.md", body: rule},
		{name: "skills/advocate/SKILL.md", body: skill},
		{name: "skills/advocate/scripts/check.sh", body: pkg.helper(), mode: 0o755},
	} {
		file.name = pkg.archiveRoot() + "/" + file.name
		file.kind = tar.TypeReg
		entries = append(entries, file)
	}
	return ffaArchive(t, entries)
}

func seedReferenceRelease(remote *dependencytest.Remote, pkg referencePackage, archive []byte) {
	release := dependency.Release{ID: pkg.release, Tag: "v1.0.0"}
	remote.Latest[pkg.source()] = release
	remote.Releases[pkg.source()+"@v1.0.0"] = release
	remote.Commits[pkg.source()+"@v1.0.0"] = pkg.commit
	remote.Archives[pkg.source()+"@"+pkg.commit] = archive
}

// TestReferenceMigrationJourneyExecutesTheIntendedHelper is the issue #92
// round-1 regression, run through the composition a consumer actually uses:
// producer migration writes the manifest, the CLI installs and realizes the
// published package, and every command the realized content instructs an
// agent to run is executed from the project directory.
//
// Two packages declare the same skill path. The owning package's legacy
// reference resolves to its own tree; its reference to the other package's
// installed tree is preserved, because redirecting it would silently run
// somebody else's helper.
func TestReferenceMigrationJourneyExecutesTheIntendedHelper(t *testing.T) {
	rule := referenceRuleBody()
	ownedManifest := convertReferencePackage(t, ownedReferencePackage, referenceSkillBody(ownedReferencePackage, unrelatedReferencePackage), rule)
	unrelatedManifest := convertReferencePackage(t, unrelatedReferencePackage, referenceSkillBody(unrelatedReferencePackage, ownedReferencePackage), rule)

	remote := dependencytest.NewRemote()
	seedReferenceRelease(remote, ownedReferencePackage, referenceArchive(t, ownedReferencePackage, ownedManifest, referenceSkillBody(ownedReferencePackage, unrelatedReferencePackage), rule))
	seedReferenceRelease(remote, unrelatedReferencePackage, referenceArchive(t, unrelatedReferencePackage, unrelatedManifest, referenceSkillBody(unrelatedReferencePackage, ownedReferencePackage), rule))

	root := t.TempDir()
	ffaRun(t, remote, root, 0, "init", "--agent", "claude-code", "--agent", "codex", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	ffaRun(t, remote, root, 0, "install", ownedReferencePackage.source(), "--non-interactive")
	ffaRun(t, remote, root, 0, "install", unrelatedReferencePackage.source(), "--non-interactive")
	ffaRun(t, remote, root, 0, "realize")
	ffaRun(t, remote, root, 0, "check")

	for _, native := range []struct {
		skills, ruleHost string
		ruleCommands     int
	}{
		// Both packages contribute a rule, so a shared Markdown host carries
		// two rule commands and Cursor's per-rule file carries the one.
		{skills: ".claude/skills", ruleHost: "CLAUDE.md", ruleCommands: 2},
		{skills: ".codex/skills", ruleHost: "AGENTS.md", ruleCommands: 2},
		{skills: ".cursor/skills", ruleHost: ".cursor/rules/acr__legacy-workspace__advocate-plugin__measurement.mdc", ruleCommands: 1},
	} {
		skill := filepath.Join(root, filepath.FromSlash(native.skills), "acr__legacy-workspace__advocate-plugin__advocate", "SKILL.md")
		realized, err := os.ReadFile(skill)
		if err != nil {
			t.Fatal(err)
		}
		commands := referenceCommands(t, string(realized))
		if len(commands) != 2 {
			t.Fatalf("%s instructs %d commands (%v), want the package-root and legacy forms", skill, len(commands), commands)
		}
		for _, command := range commands {
			assertReferenceHelper(t, root, command, ownedReferencePackage.identity)
		}
		for _, preserved := range []string{
			"`.tessl/plugins/" + unrelatedReferencePackage.identity + "/skills/advocate/scripts/check.sh`",
			"`https://example.com/?next=skills/advocate/scripts/check.sh`",
		} {
			if !strings.Contains(string(realized), preserved) {
				t.Fatalf("%s did not preserve %s:\n%s", skill, preserved, realized)
			}
		}

		// The other package is installed too, so its own helper is reachable
		// at its own native path — the preserved reference is a wrong path,
		// not a missing package.
		other, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(native.skills), "acr__other-workspace__other-plugin__advocate", "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		otherCommands := referenceCommands(t, string(other))
		if len(otherCommands) != 2 {
			t.Fatalf("the unrelated package instructs %d commands (%v)", len(otherCommands), otherCommands)
		}
		for _, command := range otherCommands {
			assertReferenceHelper(t, root, command, unrelatedReferencePackage.identity)
		}

		host, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(native.ruleHost)))
		if err != nil {
			t.Fatal(err)
		}
		ruleCommands := referenceCommands(t, string(host))
		if len(ruleCommands) != native.ruleCommands {
			t.Fatalf("%s carries %d rule commands (%v), want %d:\n%s", native.ruleHost, len(ruleCommands), ruleCommands, native.ruleCommands, host)
		}
		for _, command := range ruleCommands {
			if !strings.HasSuffix(command, "--rule") {
				t.Fatalf("%s carries an unexpected command %q", native.ruleHost, command)
			}
			assertReferenceHelper(t, root, command, referenceOwnerOf(t, command))
		}
	}
}

// referenceCommands returns every backquoted command a realized file tells an
// agent to run.
func referenceCommands(t *testing.T, content string) []string {
	t.Helper()
	var commands []string
	for _, line := range strings.Split(content, "\n") {
		const marker = "Run `"
		if !strings.HasPrefix(line, marker) {
			continue
		}
		command, _, found := strings.Cut(line[len(marker):], "`")
		if !found {
			t.Fatalf("unterminated command on line %q", line)
		}
		commands = append(commands, command)
	}
	return commands
}

// referenceOwnerOf names the package whose tree a command addresses, so a rule
// host shared between two packages is checked against the helper it actually
// points at rather than against a guess.
func referenceOwnerOf(t *testing.T, command string) string {
	t.Helper()
	for _, pkg := range []referencePackage{ownedReferencePackage, unrelatedReferencePackage} {
		if strings.Contains(command, "acr__"+strings.ReplaceAll(pkg.identity, "/", "__")+"__advocate/") {
			return pkg.identity
		}
	}
	t.Fatalf("command %q addresses neither installed package", command)
	return ""
}

func assertReferenceHelper(t *testing.T, root, command, wantHelper string) {
	t.Helper()
	fields := strings.Fields(command)
	if len(fields) != 2 {
		t.Fatalf("command %q does not carry a program and one argument", command)
	}
	executable := exec.Command(filepath.Join(root, filepath.FromSlash(fields[0])), fields[1:]...)
	executable.Dir = root
	var stdout, stderr bytes.Buffer
	executable.Stdout = &stdout
	executable.Stderr = &stderr
	if err := executable.Run(); err != nil {
		t.Fatalf("run %q from the project directory: %v\n%s%s", command, err, stdout.String(), stderr.String())
	}
	want := "{\"helper\":\"" + wantHelper + "\",\"argument\":\"" + fields[1] + "\"}\n"
	if stdout.String() != want {
		t.Fatalf("run %q stdout = %q, want %q", command, stdout.String(), want)
	}
}
