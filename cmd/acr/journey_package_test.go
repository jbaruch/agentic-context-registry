package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// journeyFile is one file inside a package fixture. Every byte is generated
// from text during setup, so no binary fixture is checked in and every
// expectation stays independently readable.
type journeyFile struct {
	path       string
	body       string
	executable bool
}

// journeyPackage is one publishable package at one version, together with the
// GitHub-shaped source tarball a consumer downloads for it.
type journeyPackage struct {
	fullName string
	source   string
	version  string
	tag      string
	commit   string
	root     string
	files    []journeyFile
	archive  []byte
}

// journeyCommit derives a stable 40-character commit for one package version.
// A fixture that recomputed it per run could not assert an immutable lock.
func journeyCommit(fullName, version string) string {
	digest := sha256.Sum256([]byte("acr-journey-commit\x00" + fullName + "\x00" + version))
	return hex.EncodeToString(digest[:])[:40]
}

// newJourneyPackage builds the alpha-shaped package: two rules with different
// activation, a skill with a reference companion and an executable script, and
// an executable session-start hook. Every version changes observable content
// so a later assertion can tell v1 from v2 without reading the lock.
func newJourneyPackage(t *testing.T, fullName, version string) *journeyPackage {
	t.Helper()
	files := []journeyFile{
		{path: "agent-plugin.yaml", body: journeyManifest(fullName, version)},
		{path: "rules/boundaries.md", body: "# " + fullName + " boundary\nVerified facts only, revision " + version + ".\n"},
		{path: "rules/scoped.md", body: "# Documentation rule\nDescribe behaviour, revision " + version + ".\n"},
		{path: "skills/advocate/SKILL.md", body: "---\nname: advocate\ndescription: Review one complaint.\n---\nRead references/guide.md and run scripts/check.sh.\nRevision " + version + ".\n"},
		{path: "skills/advocate/references/guide.md", body: "# Companion\nCheck the facts, revision " + version + ".\n"},
		{path: "skills/advocate/scripts/check.sh", body: "#!/usr/bin/env bash\nset -euo pipefail\nprintf 'companion " + version + "\\n'\n", executable: true},
		{path: "hooks/session-start.sh", body: "#!/usr/bin/env bash\nset -euo pipefail\nprintf 'session " + version + "\\n'\n", executable: true},
	}
	return newJourneyPackageFrom(t, fullName, version, files)
}

// newJourneySmallPackage builds the beta-shaped package: one always-on rule
// and one hook. It is the sibling a journey holds still while it changes
// alpha, so the two fixtures stay distinguishable in every output.
func newJourneySmallPackage(t *testing.T, fullName, version string) *journeyPackage {
	t.Helper()
	files := []journeyFile{
		{path: "agent-plugin.yaml", body: journeySmallManifest(fullName, version)},
		{path: "rules/sibling.md", body: "# " + fullName + " sibling rule\nRevision " + version + ".\n"},
		{path: "hooks/session-start.sh", body: "#!/usr/bin/env bash\nset -euo pipefail\nprintf 'sibling " + version + "\\n'\n", executable: true},
	}
	return newJourneyPackageFrom(t, fullName, version, files)
}

func newJourneyPackageFrom(t *testing.T, fullName, version string, files []journeyFile) *journeyPackage {
	t.Helper()
	commit := journeyCommit(fullName, version)
	pkg := &journeyPackage{
		fullName: fullName,
		source:   "github:" + fullName,
		version:  version,
		tag:      "v" + version,
		commit:   commit,
		root:     journeyArchiveRoot(fullName, commit),
		files:    files,
	}
	pkg.archive = journeyGitHubArchive(t, pkg.root, commit, files)
	return pkg
}

// journeyArchiveRoot reproduces the single root GitHub gives a source tarball.
func journeyArchiveRoot(fullName, commit string) string {
	return strings.ReplaceAll(fullName, "/", "-") + "-" + commit[:7]
}

// body returns one fixture file's expected content.
func (pkg *journeyPackage) body(t *testing.T, path string) string {
	t.Helper()
	for _, file := range pkg.files {
		if file.path == path {
			return file.body
		}
	}
	t.Fatalf("package %s@%s has no file %q", pkg.fullName, pkg.version, path)
	return ""
}

func journeyManifest(fullName, version string) string {
	return "schemaVersion: 1\n" +
		"name: " + fullName + "\n" +
		"version: " + version + "\n" +
		"source:\n  repository: https://github.com/" + fullName + "\n" +
		"artifacts:\n" +
		"  rules:\n" +
		"    - id: boundaries\n      path: rules/boundaries.md\n      activation:\n        mode: always\n" +
		"    - id: scoped\n      path: rules/scoped.md\n      activation:\n        mode: paths\n        paths:\n          - docs/**\n" +
		"  skills:\n    - id: advocate\n      path: skills/advocate\n" +
		"  hooks:\n    - id: session-start\n      event: session-start\n      path: hooks/session-start.sh\n"
}

func journeySmallManifest(fullName, version string) string {
	return "schemaVersion: 1\n" +
		"name: " + fullName + "\n" +
		"version: " + version + "\n" +
		"source:\n  repository: https://github.com/" + fullName + "\n" +
		"artifacts:\n" +
		"  rules:\n    - id: sibling\n      path: rules/sibling.md\n      activation:\n        mode: always\n" +
		"  hooks:\n    - id: session-start\n      event: session-start\n      path: hooks/session-start.sh\n"
}

// journeyGitHubArchive builds the archive codeload actually serves: a leading
// pax_global_header carrying the commit, one real root directory, and group
// permissions (0664 / 0775) that no publisher records. Consumers normalize
// those back to 0644 / 0755, so a fixture without them cannot prove the
// content hash a release records is reachable from a source tarball.
func journeyGitHubArchive(t *testing.T, root, commit string, files []journeyFile) []byte {
	t.Helper()
	entries := []ffaTarEntry{
		{name: "pax_global_header", kind: tar.TypeXGlobalHeader, body: ffaPAX("comment", commit)},
		{name: root + "/", kind: tar.TypeDir, mode: 0o775},
	}
	directories := map[string]bool{}
	ordered := append([]journeyFile(nil), files...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].path < ordered[right].path })
	for _, file := range ordered {
		segments := strings.Split(file.path, "/")
		for index := 1; index < len(segments); index++ {
			directory := strings.Join(segments[:index], "/")
			if directories[directory] {
				continue
			}
			directories[directory] = true
			entries = append(entries, ffaTarEntry{name: root + "/" + directory + "/", kind: tar.TypeDir, mode: 0o775})
		}
		mode := int64(0o664)
		if file.executable {
			mode = 0o775
		}
		entries = append(entries, ffaTarEntry{name: root + "/" + file.path, kind: tar.TypeReg, body: file.body, mode: mode})
	}
	return ffaArchive(t, entries)
}

// journeyPlainArchive builds the same content without PAX metadata and with
// published permissions, the shape a normalized publisher archive has. It is
// the control that keeps a PAX-specific failure distinguishable from a
// content failure.
func journeyPlainArchive(t *testing.T, root string, files []journeyFile) []byte {
	t.Helper()
	entries := []ffaTarEntry{{name: root + "/", kind: tar.TypeDir, mode: 0o755}}
	for _, file := range files {
		mode := int64(0o644)
		if file.executable {
			mode = 0o755
		}
		entries = append(entries, ffaTarEntry{name: root + "/" + file.path, kind: tar.TypeReg, body: file.body, mode: mode})
	}
	return ffaArchive(t, entries)
}

// nativeSkillDirectory is the installed native path for one package's skill.
func nativeSkillDirectory(agent, fullName, artifact string) string {
	return agent + "/skills/" + nativeArtifactName(fullName, artifact)
}

// nativeArtifactName is the adapter's generated name for one artifact.
func nativeArtifactName(fullName, artifact string) string {
	return fmt.Sprintf("acr__%s__%s", strings.ReplaceAll(fullName, "/", "__"), artifact)
}

// nativeHookExecutable is the installed native path for one package's hook.
func nativeHookExecutable(agent, fullName, artifact, basename string) string {
	return agent + "/hooks/" + nativeArtifactName(fullName, artifact) + "/" + basename
}
