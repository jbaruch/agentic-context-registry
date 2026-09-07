package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenamedProducerPublishRoundtrip walks the complete supported sequence a
// producer follows when its ACR package is hosted under a repository whose
// name differs from the Tessl plugin it was converted from.
//
// The sequence matters because two identities are involved and only one of
// them can be derived: `source.repository` must equal `https://github.com/` +
// `name`, so migration cannot be told the publication repository directly —
// it exits `invalid_source`. Migration runs under the original identity, which
// is what lets it record `source.tesslIdentity`, and the producer then edits
// only `name` and `source.repository` in the manifest it has not published
// yet. This test executes that sequence end to end and runs every command the
// realized content instructs an agent to run, so the documented procedure in
// docs/migration-producer.md cannot drift from what the CLI accepts.
func TestRenamedProducerPublishRoundtrip(t *testing.T) {
	const (
		tesslIdentity = "legacy-workspace/advocate-plugin"
		published     = "example/renamed"
	)
	remote := newJourneyGitHub(t)
	producer := newJourneyProject(t, remote)
	root := journeyTesslPlugin(t, tesslIdentity)

	own := ".tessl/plugins/" + tesslIdentity + "/skills/review/check.sh"
	foreign := ".tessl/plugins/other-workspace/other-plugin/skills/review/check.sh"
	skill := "---\nname: review\ndescription: Renamed producer fixture.\n---\n\n# Review\n\n" +
		"Run `" + own + " --skill`.\n\nRun `" + foreign + " --foreign`.\n"
	rule := "---\nalwaysApply: true\n---\n\n# Required helper\n\nRun `skills/review/check.sh --rule`.\n"
	reverify2Put(t, root, "skills/review/SKILL.md", skill, 0o644)
	reverify2Put(t, root, "skills/review/check.sh", "#!/bin/sh\nprintf '%s\\n' \"own:$1\"\n", 0o755)
	reverify2Put(t, root, "rules/always.md", rule, 0o644)

	// Step 1 — migrate under the original identity. Naming the publication
	// repository here is what fails; the reviewer reproduced that exit.
	producer.runOnPath(root, 1, "migrate", "tessl-plugin", "--repository", "https://github.com/"+published)
	producer.runOnPath(root, 0, "migrate", "tessl-plugin", "--repository", "https://github.com/"+tesslIdentity)

	converted, err := os.ReadFile(filepath.Join(root, "agent-plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), "tesslIdentity: "+tesslIdentity) {
		t.Fatalf("migration did not record the Tessl identity:\n%s", converted)
	}

	// Step 2 — author the publication identity in the unpublished manifest,
	// leaving the recorded Tessl identity alone.
	renamed := strings.Replace(string(converted), "name: "+tesslIdentity, "name: "+published, 1)
	renamed = strings.Replace(renamed, "repository: https://github.com/"+tesslIdentity, "repository: https://github.com/"+published, 1)
	if renamed == string(converted) {
		t.Fatalf("the rename edited nothing:\n%s", converted)
	}
	reverify2Put(t, root, "agent-plugin.yaml", renamed, 0o644)

	// Step 3 — commit, tag and publish through the production client.
	journeyGit(t, root, "init", "-q", "-b", "main")
	journeyGit(t, root, "add", "-A")
	journeyGit(t, root, "commit", "-qm", "Publish the renamed conversion")
	journeyGit(t, root, "tag", "v1.0.0")
	commit := journeyGit(t, root, "rev-parse", "HEAD")
	remote.PublishSource(published, "v1.0.0", commit, journeyGitSourceArchive(t, root, "v1.0.0", commit))
	producer.runOnPath(root, 0, "publish")
	if releases := len(remote.Repository(published).Releases); releases != 1 {
		t.Fatalf("publish created %d releases, want 1", releases)
	}

	// Step 4 — a consumer installs the publication and every instructed
	// command resolves to the helper it names.
	consumer := newJourneyProject(t, remote)
	consumer.run(0, "init", "--agent", "claude-code", "--agent", "codex", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	reverify2Put(t, consumer.root, foreign, "#!/bin/sh\nprintf '%s\\n' \"foreign:$1\"\n", 0o755)
	consumer.run(0, "install", "github:"+published, "--non-interactive")
	consumer.run(0, "realize")
	consumer.run(0, "check")

	for _, agent := range []string{".claude", ".codex", ".cursor"} {
		body := readProjectFile(t, consumer, nativeSkillDirectory(agent, published, "review")+"/SKILL.md")
		commands := referenceCommands(t, body)
		if len(commands) != 2 {
			t.Fatalf("%s instructs %d commands (%v), want the own and foreign helpers", agent, len(commands), commands)
		}
		assertRenamedHelper(t, consumer.root, commands[0], "own:--skill\n")
		if fields := strings.Fields(commands[1]); fields[0] != foreign {
			t.Fatalf("%s redirected the foreign reference to %q", agent, fields[0])
		}
		assertRenamedHelper(t, consumer.root, commands[1], "foreign:--foreign\n")
	}
	for _, host := range []string{"CLAUDE.md", "AGENTS.md", ".cursor/rules/acr__example__renamed__always.mdc"} {
		commands := referenceCommands(t, readProjectFile(t, consumer, host))
		if len(commands) != 1 {
			t.Fatalf("%s carries %d rule commands (%v), want one", host, len(commands), commands)
		}
		assertRenamedHelper(t, consumer.root, commands[0], "own:--rule\n")
	}
}

func assertRenamedHelper(t *testing.T, root, command, want string) {
	t.Helper()
	fields := strings.Fields(command)
	if got := runProjectHelper(t, root, fields); got != want {
		t.Fatalf("run %q = %q, want %q", command, got, want)
	}
}

// runProjectHelper executes one instructed command from the project
// directory. exec resolves a relative program against this process's
// directory, not Dir, so the join is what "run it from the project" means.
func runProjectHelper(t *testing.T, root string, fields []string) string {
	t.Helper()
	command := exec.Command(filepath.Join(root, filepath.FromSlash(fields[0])), fields[1:]...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("execute %v: %v\n%s", fields, err, output)
	}
	return string(output)
}

// TestSameNameRepublishRoundtrip covers the other branch of the same
// procedure: a package already published under its own name, republished so
// its references carry the recorded Tessl identity.
//
// The branch has one trap of its own. Conversion regenerates `version` from
// the Tessl manifest, so a republication that skips the version step commits
// the version that is already tagged and `git tag` refuses it. This walks the
// whole sequence — publish the old version, delete the manifest, convert
// again, set the new version, tag, publish — and requires the earlier tag and
// release to survive while the new version installs and runs.
func TestSameNameRepublishRoundtrip(t *testing.T) {
	const identity = "legacy-workspace/advocate-plugin"
	remote := newJourneyGitHub(t)
	producer := newJourneyProject(t, remote)
	root := journeyTesslPlugin(t, identity)

	own := ".tessl/plugins/" + identity + "/skills/review/check.sh"
	skill := "---\nname: review\ndescription: Same-name republication fixture.\n---\n\n# Review\n\n" +
		"Run `" + own + " --skill`.\n"
	rule := "---\nalwaysApply: true\n---\n\n# Required helper\n\nRun `skills/review/check.sh --rule`.\n"
	reverify2Put(t, root, "skills/review/SKILL.md", skill, 0o644)
	reverify2Put(t, root, "skills/review/check.sh", "#!/bin/sh\nprintf '%s\\n' \"own:$1\"\n", 0o755)
	reverify2Put(t, root, "rules/always.md", rule, 0o644)

	// The already-published version, from before the identity was recorded.
	producer.runOnPath(root, 0, "migrate", "tessl-plugin", "--repository", "https://github.com/"+identity)
	manifest := filepath.Join(root, "agent-plugin.yaml")
	published, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(strings.Replace(string(published), "  tesslIdentity: "+identity+"\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	journeyGit(t, root, "init", "-q", "-b", "main")
	journeyGit(t, root, "add", "-A")
	journeyGit(t, root, "commit", "-qm", "Publish the first conversion")
	journeyGit(t, root, "tag", "v1.0.0")
	firstCommit := journeyGit(t, root, "rev-parse", "HEAD")
	remote.PublishSource(identity, "v1.0.0", firstCommit, journeyGitSourceArchive(t, root, "v1.0.0", firstCommit))
	producer.runOnPath(root, 0, "publish")

	// Step 1 — delete, then step 2 — convert again under the same identity.
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	producer.runOnPath(root, 0, "migrate", "tessl-plugin", "--repository", "https://github.com/"+identity)
	converted, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), "tesslIdentity: "+identity) {
		t.Fatalf("republished conversion did not record the Tessl identity:\n%s", converted)
	}
	// Step 5 is what the same-name branch used to skip: conversion regenerated
	// the published version, so tagging it again would fail.
	if !strings.Contains(string(converted), "version: 1.0.0") {
		t.Fatalf("conversion did not regenerate the published version:\n%s", converted)
	}
	bumped := strings.Replace(string(converted), "version: 1.0.0", "version: 1.1.0", 1)
	if err := os.WriteFile(manifest, []byte(bumped), 0o644); err != nil {
		t.Fatal(err)
	}

	journeyGit(t, root, "add", "-A")
	journeyGit(t, root, "commit", "-qm", "Republish with the recorded identity")
	journeyGit(t, root, "tag", "v1.1.0")
	secondCommit := journeyGit(t, root, "rev-parse", "HEAD")
	remote.PublishSource(identity, "v1.1.0", secondCommit, journeyGitSourceArchive(t, root, "v1.1.0", secondCommit))
	producer.runOnPath(root, 0, "publish")

	repository := remote.Repository(identity)
	if len(repository.Releases) != 2 {
		t.Fatalf("republication left %d releases, want the old one and the new one", len(repository.Releases))
	}
	if repository.Tags["v1.0.0"] != firstCommit {
		t.Fatalf("the earlier tag moved: %q, want %q", repository.Tags["v1.0.0"], firstCommit)
	}

	consumer := newJourneyProject(t, remote)
	consumer.run(0, "init", "--agent", "claude-code", "--agent", "codex", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	consumer.run(0, "install", "github:"+identity, "--non-interactive")
	consumer.run(0, "realize")
	consumer.run(0, "check")

	for _, agent := range []string{".claude", ".codex", ".cursor"} {
		body := readProjectFile(t, consumer, nativeSkillDirectory(agent, identity, "review")+"/SKILL.md")
		commands := referenceCommands(t, body)
		if len(commands) != 1 {
			t.Fatalf("%s instructs %d commands (%v), want the legacy reference alone", agent, len(commands), commands)
		}
		assertRenamedHelper(t, consumer.root, commands[0], "own:--skill\n")
	}
	for _, host := range []string{"CLAUDE.md", "AGENTS.md", ".cursor/rules/acr__legacy-workspace__advocate-plugin__always.mdc"} {
		commands := referenceCommands(t, readProjectFile(t, consumer, host))
		if len(commands) != 1 {
			t.Fatalf("%s carries %d rule commands (%v), want one", host, len(commands), commands)
		}
		assertRenamedHelper(t, consumer.root, commands[0], "own:--rule\n")
	}
}
