package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// producerJourneys covers publication and both migration targets: the
// commands a package author runs, and the ones a project runs to leave Tessl.
func producerJourneys() []journeyCase {
	return []journeyCase{
		{leaf: "publish", name: "consumer-roundtrip", kind: journeySuccess, run: journeyPublishRoundtrip},
		{leaf: "publish", name: "immutability-and-worktree", kind: journeyRefusal, run: journeyPublishRefusals},
		{leaf: "migrate tessl-plugin", name: "convert-publish-consume", kind: journeySuccess, run: journeyMigrateProducerSuccess},
		{leaf: "migrate tessl-plugin", name: "missing-repository", kind: journeyRefusal, run: journeyMigrateProducerRefusals},
		{leaf: "migrate tessl", name: "mapped-vendor-and-finalize", kind: journeySuccess, run: journeyMigrateConsumerSuccess},
		{leaf: "migrate tessl", name: "unmapped-package", kind: journeyRefusal, run: journeyMigrateConsumerRefusals},
	}
}

// journeyPublishRoundtrip is the whole producer-to-consumer arc: a real tagged
// Git repository is published through the GitHub boundary, and a consumer that
// has never seen it installs, realizes and checks it. Nothing seeds the
// release the consumer resolves; publishing creates it.
func journeyPublishRoundtrip(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	repository, commit := journeyPublisher(t, alpha)
	github.PublishSource(alpha.fullName, alpha.tag, commit, journeyGitSourceArchive(t, repository, alpha.tag, commit))

	producer := newJourneyProject(t, github)
	github.ResetRequests()
	dry := producer.runOnPath(repository, 0, "publish", "--dry-run", "--json")
	plan := journeyResult(t, dry.stdout)
	assets, ok := plan["assets"].([]any)
	if !ok || len(assets) != 3 {
		t.Fatalf("acr publish --dry-run assets = %#v, want three", plan["assets"])
	}
	if plan["tag"] != alpha.tag || plan["commit"] != commit || plan["dryRun"] != true {
		t.Fatalf("acr publish --dry-run = %#v, want the tagged identity", plan)
	}
	contentHash, _ := plan["contentHash"].(string)
	if !strings.HasPrefix(contentHash, "sha256:") {
		t.Fatalf("acr publish --dry-run content hash = %q", contentHash)
	}
	for _, request := range github.Requests() {
		if strings.HasPrefix(request, "POST") || strings.HasPrefix(request, "PATCH") || strings.HasPrefix(request, "DELETE") {
			t.Fatalf("acr publish --dry-run wrote to GitHub: %v", github.Requests())
		}
	}
	if released := github.Repository(alpha.fullName).Releases; len(released) != 0 {
		t.Fatalf("acr publish --dry-run created %#v", released)
	}
	worktree := snapshotProjectTree(t, repository)

	github.ResetRequests()
	applied := producer.runOnPath(repository, 0, "publish", "--json")
	assertNoCredentialLeak(t, applied)
	result := journeyResult(t, applied.stdout)
	if result["dryRun"] != false || result["contentHash"] != contentHash {
		t.Fatalf("acr publish = %#v, want the rehearsed identity", result)
	}
	if identifier, _ := result["releaseId"].(float64); identifier <= 0 {
		t.Fatalf("acr publish result = %#v, want the created release id", result)
	}
	assertTreeUnchanged(t, worktree, repository, "acr publish")

	// The draft was created, every asset was uploaded and read back, and the
	// release became visible only afterwards.
	if github.RequestCount("POST https://"+journeyUploadHost) != 3 {
		t.Fatalf("acr publish uploaded %d assets: %v", github.RequestCount("POST https://"+journeyUploadHost), github.Requests())
	}
	if github.RequestCount("PATCH https://"+journeyAPIHost) != 1 {
		t.Fatalf("acr publish published %d times: %v", github.RequestCount("PATCH https://"+journeyAPIHost), github.Requests())
	}
	if github.AuthorizationSeenOn(journeyAssetHost) {
		t.Fatal("an asset redirect carried the credential to the download origin")
	}
	releases := github.Repository(alpha.fullName).Releases
	if len(releases) != 1 || releases[0].Draft || len(releases[0].Assets) != 3 {
		t.Fatalf("published release = %#v, want one visible release with three assets", releases)
	}
	names := map[string]bool{}
	for _, asset := range releases[0].Assets {
		names[asset.Name] = true
		if len(asset.Bytes) == 0 {
			t.Fatalf("release asset %q is empty", asset.Name)
		}
	}
	if !names[dependency.ReleaseMetadataAssetName] {
		t.Fatalf("published assets = %v, want the package metadata", names)
	}
	var metadata struct {
		MetadataVersion int    `json:"metadataVersion"`
		Commit          string `json:"commit"`
		ContentHash     string `json:"contentHash"`
	}
	for _, asset := range releases[0].Assets {
		if asset.Name == dependency.ReleaseMetadataAssetName {
			if err := json.Unmarshal(asset.Bytes, &metadata); err != nil {
				t.Fatalf("decode %s: %v", asset.Name, err)
			}
		}
	}
	if metadata.MetadataVersion != 1 || metadata.Commit != commit || metadata.ContentHash != contentHash {
		t.Fatalf("release metadata = %#v, want the published identity", metadata)
	}

	// A consumer that has never seen this package installs what was published.
	consumer := newJourneyProject(t, github)
	consumer.run(0, "init", "--agent", "claude-code", "--freshness", "none", "--non-interactive")
	consumer.run(0, "install", alpha.source, "--non-interactive")
	locked := loadJourneyState(t, consumer).Lock.Dependencies
	if len(locked) != 1 || locked[0].Commit != commit || locked[0].ContentHash != contentHash || locked[0].Tag != alpha.tag {
		t.Fatalf("consumer lock = %#v, want the publisher's commit %s and hash %s", locked, commit, contentHash)
	}
	listed := consumer.run(0, "list")
	if !strings.Contains(listed.stdout, alpha.source) {
		t.Fatalf("acr list = %q, want the installed package", listed.stdout)
	}
	consumer.run(0, "realize")
	skill := nativeSkillDirectory(".claude", alpha.fullName, "advocate")
	assertProjectFile(t, consumer, skill+"/SKILL.md", alpha.body(t, "skills/advocate/SKILL.md"), 0o644)
	assertProjectFile(t, consumer, skill+"/scripts/check.sh", alpha.body(t, "skills/advocate/scripts/check.sh"), 0o755)
	assertProjectFile(t, consumer, nativeHookExecutable(".claude", alpha.fullName, "session-start", "session-start.sh"),
		alpha.body(t, "hooks/session-start.sh"), 0o755)
	consumer.run(0, "check")
	return len(releases[0].Assets) + 1
}

func journeyPublishRefusals(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneyPackage(t, "example/alpha", "1.0.0")
	repository, commit := journeyPublisher(t, alpha)
	producer := newJourneyProject(t, github)

	refusals := 0
	// The tag is not on GitHub yet.
	before := snapshotProjectTree(t, repository)
	notPushed := producer.runOnPath(repository, 1, "publish", "--json")
	if journeyError(t, notPushed.stderr)["code"] != "tag_not_pushed" {
		t.Fatalf("publish before the tag was pushed = %q", notPushed.stderr)
	}
	refusals++

	github.PublishSource(alpha.fullName, alpha.tag, commit, journeyGitSourceArchive(t, repository, alpha.tag, commit))

	// A dirty worktree refuses before any remote write.
	reverify2Put(t, repository, "scratch.md", "# uncommitted\n", 0o644)
	github.ResetRequests()
	dirty := producer.runOnPath(repository, 1, "publish", "--json")
	if journeyError(t, dirty.stderr)["code"] != "dirty_worktree" {
		t.Fatalf("publish from a dirty worktree = %q", dirty.stderr)
	}
	if len(github.Requests()) != 0 {
		t.Fatalf("a dirty worktree still reached GitHub: %v", github.Requests())
	}
	refusals++
	if err := os.Remove(repository + "/scratch.md"); err != nil {
		t.Fatal(err)
	}

	// An untagged commit has nothing publishable.
	untagged, _ := journeyPublisher(t, newJourneySmallPackage(t, "example/beta", "1.0.0"))
	journeyGit(t, untagged, "tag", "-d", "v1.0.0")
	noTag := producer.runOnPath(untagged, 1, "publish", "--json")
	if journeyError(t, noTag.stderr)["code"] != "no_publishable_tag" {
		t.Fatalf("publish without a tag = %q", noTag.stderr)
	}
	refusals++

	// A directory with no manifest is not a package.
	empty := producer.runOnPath(t.TempDir(), 1, "publish", "--json")
	if !strings.Contains(journeyError(t, empty.stderr)["message"].(string), "agent-plugin.yaml") {
		t.Fatalf("publish without a manifest = %q", empty.stderr)
	}
	refusals++

	// A published release is immutable: the same version is never overwritten.
	producer.runOnPath(repository, 0, "publish")
	assets := len(github.Repository(alpha.fullName).Releases[0].Assets)
	github.ResetRequests()
	duplicate := producer.runOnPath(repository, 1, "publish", "--json")
	if journeyError(t, duplicate.stderr)["code"] != "release_already_exists" {
		t.Fatalf("a duplicate publish = %q, want release_already_exists", duplicate.stderr)
	}
	if github.RequestCount("POST https://"+journeyUploadHost) != 0 {
		t.Fatalf("a refused duplicate publish uploaded assets: %v", github.Requests())
	}
	if now := github.Repository(alpha.fullName).Releases; len(now) != 1 || len(now[0].Assets) != assets {
		t.Fatalf("a refused duplicate publish changed the release: %#v", now)
	}
	refusals++
	assertTreeUnchanged(t, before, repository, "a refused publish")
	return refusals
}

// journeyTesslPlugin writes a Tessl plugin package whose manifest carries no
// repository, the case the producer migration exists to convert.
func journeyTesslPlugin(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	reverify2Put(t, root, ".tessl-plugin/plugin.json",
		`{"name":"`+name+`","version":"1.0.0","description":"Journey fixture",`+
			`"rules":["rules/always.md"],"skills":["skills/review"],`+
			`"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash",`+
			`"args":["${TESSL_PLUGIN_DIR}/hooks/session-start.sh"]}]}]}}`+"\n", 0o644)
	reverify2Put(t, root, "rules/always.md", "---\nalwaysApply: true\n---\n# Converted rule\nKeep the facts straight.\n", 0o644)
	reverify2Put(t, root, "skills/review/SKILL.md", "---\nname: review\ndescription: Review a change.\n---\n# Review\nRead the diff.\n", 0o644)
	reverify2Put(t, root, "hooks/session-start.sh", "#!/usr/bin/env bash\nset -euo pipefail\nprintf 'converted session\\n'\n", 0o755)
	return root
}

// journeyMigrateProducerSuccess converts a Tessl plugin that has no repository
// metadata, publishes the conversion from a real tag, and consumes it.
func journeyMigrateProducerSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	producer := newJourneyProject(t, github)
	root := journeyTesslPlugin(t, "example/converted")

	before := snapshotProjectTree(t, root)
	dry := producer.runOnPath(root, 0, "migrate", "tessl-plugin", "--repository", "https://github.com/example/converted", "--dry-run", "--json")
	report := journeyResult(t, dry.stdout)
	if report["wrote"] != false {
		t.Fatalf("acr migrate tessl-plugin --dry-run = %#v, want a rehearsal", report)
	}
	if !strings.Contains(dry.stdout, "review") || !strings.Contains(dry.stdout, "always") {
		t.Fatalf("acr migrate tessl-plugin --dry-run = %q, want the artifacts it found", dry.stdout)
	}
	assertTreeUnchanged(t, before, root, "acr migrate tessl-plugin --dry-run")

	producer.runOnPath(root, 0, "migrate", "tessl-plugin", "--repository", "https://github.com/example/converted")
	converted, err := os.ReadFile(root + "/agent-plugin.yaml")
	if err != nil {
		t.Fatalf("conversion wrote no manifest: %v", err)
	}
	for _, want := range []string{"name: example/converted", "version: 1.0.0", "https://github.com/example/converted", "rules/always.md", "skills/review", "hooks/session-start.sh"} {
		if !strings.Contains(string(converted), want) {
			t.Fatalf("converted manifest = %s, want it to declare %q", converted, want)
		}
	}
	// The Tessl sources the conversion read are left exactly as they were.
	assertFileBody(t, root, "rules/always.md", "---\nalwaysApply: true\n---\n# Converted rule\nKeep the facts straight.\n")

	// The converted package publishes and installs like any other.
	journeyGit(t, root, "init", "-q", "-b", "main")
	journeyGit(t, root, "add", "-A")
	journeyGit(t, root, "commit", "-qm", "Convert to agent-plugin.yaml")
	journeyGit(t, root, "tag", "v1.0.0")
	commit := journeyGit(t, root, "rev-parse", "HEAD")
	github.PublishSource("example/converted", "v1.0.0", commit, journeyGitSourceArchive(t, root, "v1.0.0", commit))
	producer.runOnPath(root, 0, "publish")

	consumer := newJourneyProject(t, github)
	consumer.run(0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	consumer.run(0, "install", "github:example/converted", "--non-interactive")
	consumer.run(0, "realize")
	assertProjectFile(t, consumer, nativeSkillDirectory(".codex", "example/converted", "review")+"/SKILL.md",
		"---\nname: review\ndescription: Review a change.\n---\n# Review\nRead the diff.\n", 0o644)
	assertProjectFile(t, consumer, nativeHookExecutable(".codex", "example/converted", "session-start", "session-start.sh"),
		"#!/usr/bin/env bash\nset -euo pipefail\nprintf 'converted session\\n'\n", 0o755)
	agents := readProjectFile(t, consumer, "AGENTS.md")
	if !strings.Contains(agents, "Keep the facts straight.") {
		t.Fatalf("AGENTS.md does not carry the converted rule: %s", agents)
	}
	consumer.run(0, "check")

	// A second conversion of the same package is the same conversion.
	settled := snapshotProjectTree(t, root)
	producer.runOnPath(root, 0, "migrate", "tessl-plugin", "--repository", "https://github.com/example/converted")
	assertTreeUnchanged(t, settled, root, "a repeated conversion")
	return 3
}

func journeyMigrateProducerRefusals(t *testing.T) int {
	producer := newJourneyProject(t, nil)
	root := journeyTesslPlugin(t, "example/converted")
	before := snapshotProjectTree(t, root)

	missing := producer.runOnPath(root, 1, "migrate", "tessl-plugin", "--json")
	failure := journeyError(t, missing.stderr)
	if failure["field"] != "source.repository" {
		t.Fatalf("conversion without repository evidence = %#v, want it to name source.repository", failure)
	}
	assertTreeUnchanged(t, before, root, "a conversion refusal")

	unknown := journeyTesslPlugin(t, "example/unknown-field")
	reverify2Put(t, unknown, ".tessl-plugin/plugin.json",
		`{"name":"example/unknown-field","version":"1.0.0","surprise":true,"rules":["rules/always.md"]}`+"\n", 0o644)
	unknownBefore := snapshotProjectTree(t, unknown)
	rejected := producer.runOnPath(unknown, 1, "migrate", "tessl-plugin", "--repository", "https://github.com/example/unknown-field", "--json")
	if !strings.Contains(rejected.stderr, "surprise") {
		t.Fatalf("conversion of an unknown field = %q, want it to name the field", rejected.stderr)
	}
	assertTreeUnchanged(t, unknownBefore, unknown, "a conversion refusal")

	absent := producer.runOnPath(t.TempDir(), 1, "migrate", "tessl-plugin", "--repository", "https://github.com/example/absent", "--json")
	if journeyError(t, absent.stderr)["code"] == "" {
		t.Fatalf("conversion of a directory with no plugin = %q", absent.stderr)
	}
	return 3
}

// journeyTesslConsumer writes a Tessl consumer holding two installed packages.
// The first carries exactly the artifacts its published ACR package carries,
// so a mapping can converge; the second has no upstream at all and only
// vendoring can preserve it.
func journeyTesslConsumer(t *testing.T, project *journeyProject, mapped *journeyPackage) {
	t.Helper()
	root := project.root
	reverify2Put(t, root, "tessl.json",
		`{"name":"consumer","dependencies":{"`+mapped.fullName+`":{"version":"`+mapped.version+`"},"example/orphan":{"version":"1.0.0"}}}`+"\n", 0o644)
	reverify2Put(t, root, "notes.md", "# Operator notes\n", 0o644)

	mappedBase := ".tessl/plugins/" + mapped.fullName
	reverify2Put(t, root, mappedBase+"/.tessl-plugin/plugin.json",
		`{"name":"`+mapped.fullName+`","version":"`+mapped.version+`","rules":["rules/sibling.md"],`+
			`"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash",`+
			`"args":["${TESSL_PLUGIN_DIR}/hooks/session-start.sh"]}]}]}}`+"\n", 0o644)
	reverify2Put(t, root, mappedBase+"/rules/sibling.md", mapped.body(t, "rules/sibling.md"), 0o644)
	reverify2Put(t, root, mappedBase+"/hooks/session-start.sh", mapped.body(t, "hooks/session-start.sh"), 0o755)

	orphanBase := ".tessl/plugins/example/orphan"
	reverify2Put(t, root, orphanBase+"/.tessl-plugin/plugin.json",
		`{"name":"example/orphan","version":"1.0.0","rules":["rules/always.md"],"skills":["skills/review-orphan"]}`+"\n", 0o644)
	reverify2Put(t, root, orphanBase+"/rules/always.md", "# Tessl orphan rule\n", 0o644)
	reverify2Put(t, root, orphanBase+"/skills/review-orphan/SKILL.md", "# Review orphan\n", 0o644)
	if err := os.MkdirAll(project.path(".codex/skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The tessl__ native link is what identifies the skill as Tessl-owned.
	if err := os.Symlink("../../.tessl/plugins/example/orphan/skills/review-orphan",
		project.path(".codex/skills/tessl__review-orphan")); err != nil {
		t.Fatal(err)
	}
	verify8GitCommit(t, root)
}

// journeyMigrateConsumerSuccess maps one Tessl package onto its published ACR
// release, vendors the one that has no upstream, and finalizes.
func journeyMigrateConsumerSuccess(t *testing.T) int {
	github := newJourneyGitHub(t)
	alpha := newJourneySmallPackage(t, "example/alpha", "1.0.0")
	github.SeedRelease(alpha.fullName, alpha.tag, alpha.commit, alpha.archive)

	project := newJourneyProject(t, github)
	journeyTesslConsumer(t, project, alpha)
	before := project.snapshot()

	dry := project.run(0, "migrate", "tessl", "--map", "example/alpha="+alpha.source+"@"+alpha.tag,
		"--vendor-unmapped", "--non-interactive", "--dry-run", "--json")
	report := journeyResult(t, dry.stdout)
	if report["wrote"] != false {
		t.Fatalf("acr migrate tessl --dry-run = %#v, want a rehearsal", report)
	}
	mappings, ok := report["mappings"].([]any)
	if !ok || len(mappings) != 2 {
		t.Fatalf("acr migrate tessl --dry-run mappings = %#v, want both installed packages", report["mappings"])
	}
	origins := map[string]string{}
	for _, entry := range mappings {
		mapping, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("mapping = %#v", entry)
		}
		origins[mapping["from"].(string)], _ = mapping["source"].(string)
	}
	if origins["example/alpha"] != alpha.source || origins["example/orphan"] != "vendor:example/orphan" {
		t.Fatalf("mappings = %#v, want alpha mapped upstream and orphan vendored", origins)
	}
	// The rehearsal classifies what each side owns; an equivalent mapping
	// leaves no effective difference to report, which is what lets the later
	// finalization clear its gate.
	if owned, _ := report["tesslOwned"].([]any); len(owned) < 3 {
		t.Fatalf("acr migrate tessl --dry-run tesslOwned = %#v, want the packages, the manifest and the native link", report["tesslOwned"])
	}
	if owned, _ := report["toolOwned"].([]any); len(owned) == 0 {
		t.Fatal("acr migrate tessl --dry-run claimed no ACR-owned output")
	}
	if diffs, _ := report["effectiveDiffs"].([]any); len(diffs) != 0 {
		t.Fatalf("the mapped package is not equivalent: %#v", diffs)
	}
	if vendored, _ := report["vendored"].([]any); len(vendored) != 1 {
		t.Fatalf("acr migrate tessl --dry-run vendored = %#v, want the unmapped package", report["vendored"])
	}
	project.assertUnchanged(before, "acr migrate tessl --dry-run")

	project.run(0, "migrate", "tessl", "--map", "example/alpha="+alpha.source+"@"+alpha.tag,
		"--vendor-unmapped", "--non-interactive")
	state := loadJourneyState(t, project)
	if len(state.Lock.Dependencies) != 2 {
		t.Fatalf("migration locked %#v, want the mapped and the vendored package", state.Lock.Dependencies)
	}
	sources := map[string]string{}
	for _, locked := range state.Lock.Dependencies {
		sources[locked.Source] = locked.Commit
	}
	if sources[alpha.source] != alpha.commit {
		t.Fatalf("the mapped package locked %#v, want the immutable %s", sources, alpha.commit)
	}
	if _, vendored := sources["vendor:example/orphan"]; !vendored {
		t.Fatalf("the unmapped package was not vendored: %#v", sources)
	}
	assertProjectFile(t, project, ".agents/vendor/example/orphan/rules/always.md", "# Tessl orphan rule\n", 0o644)
	// Tessl's own tree and the operator's file are untouched by coexistence.
	assertProjectFile(t, project, ".tessl/plugins/example/alpha/rules/sibling.md", alpha.body(t, "rules/sibling.md"), 0o644)
	assertProjectFile(t, project, "notes.md", "# Operator notes\n", 0o644)

	project.run(0, "realize")
	assertProjectFile(t, project, nativeHookExecutable(".codex", alpha.fullName, "session-start", "session-start.sh"),
		alpha.body(t, "hooks/session-start.sh"), 0o755)
	assertProjectFile(t, project, nativeSkillDirectory(".codex", "example/orphan", "review-orphan")+"/SKILL.md",
		"# Review orphan\n", 0o644)
	project.run(0, "check")

	// Finalization needs the migration committed, then removes only Tessl.
	verify8GitCommit(t, project.root)
	finalize := []string{"migrate", "tessl", "--map", "example/alpha=" + alpha.source + "@" + alpha.tag, "--vendor-unmapped", "--finalize", "--non-interactive"}
	finalizeDry := project.run(0, append(append([]string(nil), finalize...), "--dry-run", "--json")...)
	removals := journeyResult(t, finalizeDry.stdout)
	if removals["wrote"] != false {
		t.Fatalf("acr migrate tessl --finalize --dry-run = %#v", removals)
	}
	project.run(0, finalize...)
	assertProjectAbsent(t, project, ".tessl/plugins/example/alpha/rules/sibling.md")
	assertProjectAbsent(t, project, "tessl.json")
	assertProjectFile(t, project, ".agents/vendor/example/orphan/rules/always.md", "# Tessl orphan rule\n", 0o644)
	assertProjectFile(t, project, "notes.md", "# Operator notes\n", 0o644)
	project.run(0, "check")

	settled := project.snapshot()
	project.run(0, finalize...)
	project.assertUnchanged(settled, "a repeated finalization")
	return len(state.Lock.Dependencies) + 1
}

func journeyMigrateConsumerRefusals(t *testing.T) int {
	github := newJourneyGitHub(t)
	project := newJourneyProject(t, github)
	journeyTesslConsumer(t, project, newJourneySmallPackage(t, "example/alpha", "1.0.0"))
	before := project.snapshot()

	unmapped := project.run(1, "migrate", "tessl", "--non-interactive", "--json")
	if journeyError(t, unmapped.stderr)["code"] != "unmapped_package" {
		t.Fatalf("migration without a mapping = %q, want unmapped_package", unmapped.stderr)
	}
	project.assertUnchanged(before, "an unmapped migration")

	missingFile := project.run(1, "migrate", "tessl", "--mapping-file", "absent.yaml", "--non-interactive", "--json")
	if journeyError(t, missingFile.stderr)["code"] == "" {
		t.Fatalf("migration with an absent mapping file = %q", missingFile.stderr)
	}
	project.assertUnchanged(before, "a migration with an absent mapping file")

	early := project.run(4, "migrate", "tessl", "--finalize", "--non-interactive", "--json")
	if journeyError(t, early.stderr)["code"] != "finalization_blocked" {
		t.Fatalf("finalizing before migrating = %q, want finalization_blocked", early.stderr)
	}
	project.assertUnchanged(before, "a blocked finalization")
	return 3
}

func assertFileBody(t *testing.T, root, path, body string) {
	t.Helper()
	contents, err := os.ReadFile(root + "/" + path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != body {
		t.Fatalf("%s = %q, want %q", path, contents, body)
	}
}
