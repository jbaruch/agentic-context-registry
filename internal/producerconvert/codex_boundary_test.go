package producerconvert

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// codexBoundaryRead runs one read through a real wrapper and returns what the
// reader printed and whether it failed. Sentinel reads must fail with empty
// stdout; the errno text is deliberately not asserted because Seatbelt says
// "Operation not permitted" and an absent bind says "No such file".
func codexBoundaryRead(t *testing.T, argv []string, env []string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = env
	stdout, err := command.Output()
	return string(stdout), err != nil
}

// TestCodexReadBoundaryDeniesInheritedInstructionsNatively runs the production
// boundary builders through the host's real mechanism. On darwin the profile
// must deny instruction and skill files by name and the managed system roots
// by subpath while reading ordinary files; on linux the allow-list view must
// make every unbound path absent while the schema stays readable. A missing
// mechanism on a supported platform is a failure, never a skip.
func TestCodexReadBoundaryDeniesInheritedInstructionsNatively(t *testing.T) {
	switch runtime.GOOS {
	case "darwin":
		codexDarwinBoundaryNatively(t)
	case "linux":
		codexLinuxBoundaryNatively(t)
	default:
		t.Skipf("no verified Codex read boundary on %s", runtime.GOOS)
	}
}

func codexDarwinBoundaryNatively(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Fatalf("macOS boundary mechanism unavailable: %v", err)
	}
	// Seatbelt matches resolved paths, so the fixture root is canonicalized the
	// way production canonicalizes the configured home.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	system := filepath.Join(root, "system-codex")
	for name, body := range map[string]string{"home/AGENTS.md": "home instructions\n", "home/AGENTS.override.md": "override\n", "home/skills/leak/SKILL.md": "skill\n", "home/NOTAGENTS.md": "ordinary\n", "work/proposal-schema.json": proposalSchema, "system-codex/config.toml": "managed\n", "system-codex/skills/leak/SKILL.md": "system skill\n"} {
		put(t, root, name, body, 0o644)
	}
	profile := filepath.Join(root, "boundary.sb")
	if err := os.WriteFile(profile, []byte(codexDarwinProfile([]string{filepath.Join(home, "AGENTS.md"), filepath.Join(home, "AGENTS.override.md")}, append([]string{system}, codexDarwinSystemRoots...))), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	for _, denied := range []string{filepath.Join(home, "AGENTS.md"), filepath.Join(home, "AGENTS.override.md"), filepath.Join(home, "skills/leak/SKILL.md"), filepath.Join(system, "config.toml"), filepath.Join(system, "skills/leak/SKILL.md")} {
		if stdout, failed := codexBoundaryRead(t, []string{"/usr/bin/sandbox-exec", "-f", profile, "/bin/cat", denied}, env); !failed || stdout != "" {
			t.Fatalf("%s readable through the profile: failed=%t stdout=%q", denied, failed, stdout)
		}
	}
	for path, want := range map[string]string{filepath.Join(home, "NOTAGENTS.md"): "ordinary\n", filepath.Join(root, "work/proposal-schema.json"): proposalSchema} {
		if stdout, failed := codexBoundaryRead(t, []string{"/usr/bin/sandbox-exec", "-f", profile, "/bin/cat", path}, env); failed || stdout != want {
			t.Fatalf("%s not readable through the profile: failed=%t stdout=%q", path, failed, stdout)
		}
	}
	// The same profile still lets the executable start: /bin/echo stands in
	// for the Codex binary the production positive control runs.
	if stdout, failed := codexBoundaryRead(t, []string{"/usr/bin/sandbox-exec", "-f", profile, "/bin/echo", "codex-cli 0.154.0"}, env); failed || stdout != "codex-cli 0.154.0\n" {
		t.Fatalf("positive control failed through the profile: failed=%t stdout=%q", failed, stdout)
	}
}

func codexLinuxBoundaryNatively(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatalf("Linux boundary mechanism unavailable: %v; install bubblewrap", err)
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	caBundle, err := codexCABundle(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "iso-home")
	operator := filepath.Join(root, "operator-home")
	for name, body := range map[string]string{"work/proposal-schema.json": proposalSchema, "iso-home/.codex/keep": "", "operator-home/.codex/AGENTS.md": "S1\n", "operator-home/.codex/AGENTS.override.md": "S2\n", "operator-home/.codex/skills/leak/SKILL.md": "S3\n", "operator-home/.agents/skills/leak/SKILL.md": "S4\n", "operator-home/AGENTS.md": "S5\n", "operator-home/project/.codex/AGENTS.md": "S6\n", "canary/AGENTS.md": "S7\n"} {
		put(t, root, name, body, 0o644)
	}
	if err := os.Symlink(filepath.Join(operator, ".codex"), filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	// The reader view is the production view plus what a dynamically linked
	// cat needs; the production view is a strict subset, so every absence
	// proven here holds for the provider too.
	reader := func(chdir string) []string {
		view := append([]string{bwrap}, codexLinuxView(cat, caBundle, work, home, chdir)...)
		return append(view, "--ro-bind", "/usr/lib", "/usr/lib", "--ro-bind-try", "/lib", "/lib", "--ro-bind-try", "/lib64", "/lib64", "--")
	}
	env := codexBoundaryEnvironment("/opt/acr", home, work, nil, "SSL_CERT_FILE="+caBundle)
	if stdout, failed := codexBoundaryRead(t, append(reader(work), codexLinuxViewTarget, filepath.Join(work, "proposal-schema.json")), env); failed || stdout != proposalSchema {
		t.Fatalf("positive control failed in the view: failed=%t stdout=%q (namespace hint: %s)", failed, stdout, codexLinuxNamespaceHint(stdout))
	}
	for _, sentinel := range []string{filepath.Join(operator, ".codex/AGENTS.md"), filepath.Join(operator, ".codex/AGENTS.override.md"), filepath.Join(operator, ".codex/skills/leak/SKILL.md"), filepath.Join(operator, ".agents/skills/leak/SKILL.md"), filepath.Join(operator, "AGENTS.md"), filepath.Join(operator, "project/.codex/AGENTS.md"), filepath.Join(root, "canary/AGENTS.md"), filepath.Join(work, "escape/AGENTS.md"), "/etc/codex/AGENTS.md", "/proc/1/root" + filepath.Join(operator, ".codex/AGENTS.md"), "/usr/bin/env"} {
		if stdout, failed := codexBoundaryRead(t, append(reader(work), codexLinuxViewTarget, sentinel), env); !failed || stdout != "" {
			t.Fatalf("%s reachable in the view: failed=%t stdout=%q", sentinel, failed, stdout)
		}
	}
	// The negative canary the production run performs: an unbound directory
	// cannot become the working directory.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, bwrap, append(codexLinuxView(cat, caBundle, work, home, filepath.Join(root, "canary")), "--", codexLinuxViewTarget, "/dev/null")...)
	command.Env = env
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Can't chdir to "+filepath.Join(root, "canary")) {
		t.Fatalf("unbound canary directory was enterable: %v %s", err, output)
	}
}

// TestCodexDarwinSeatbeltRunsScriptedProvider drives the full production flow
// through the real macOS wrapper with the scripted provider, so the canary
// denial and the positive path are proven with the shipped profile rather
// than the fake wrapper.
func TestCodexDarwinSeatbeltRunsScriptedProvider(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Seatbelt only")
	}
	root, options, proposed := semanticFixture(t)
	options.Agent = "codex"
	native := codexFixtureFor(t, proposed, "darwin")
	native.wrapper = "/usr/bin/sandbox-exec"
	before := treeAt(t, root)
	plan, err := prepareWithProvider(context.Background(), options, func(ctx context.Context, _ string, request string) (proposal, AgentRun, error) {
		return runCodexWithRuntime(ctx, request, native)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !matches(before, treeAt(t, root)) || len(plan.Report.AgentRuns) != 1 || !strings.Contains(plan.Report.AgentRuns[0].Isolation, `(subpath "/etc/codex")`) {
		t.Fatalf("seatbelt run evidence: %+v", plan.Report.AgentRuns)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if read(t, root, proposed.Edits[0].Path) != proposed.Edits[0].Content {
		t.Fatal("proposal not applied through the real wrapper")
	}
}
