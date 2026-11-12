package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the shipped helper with all host commands replaced. Only absolute host
// policy paths are projected into a fixture, so no test can touch host policy.
func TestBwrapProfileSetupFailsClosed(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "scripts", "setup-bwrap-profile.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"success", "wrong-platform", "root-runner", "wrong-package", "wrong-path", "wrong-owner", "setuid", "existing-profile", "competing-profile", "override", "download", "archive-hash", "package-metadata", "profile-hash", "compile", "compile-warning", "load", "load-warning", "missing-enforce", "after-probe"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			policy := filepath.Join(root, "policy")
			for _, path := range []string{bin, policy, filepath.Join(policy, "local")} {
				if err := os.MkdirAll(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "override" {
				if err := os.WriteFile(filepath.Join(policy, "local", "unpriv_bwrap"), []byte("override"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(string(data), "/etc/apparmor.d", policy)
			scriptPath := filepath.Join(root, "setup.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			// Bash builtins implement mocks; no installed apt, sudo, parser or Go runs.
			mock := `#!/bin/bash
set -euo pipefail
name="${0##*/}"
printf '%s %s\n' "$name" "$*" >> "$TEST_LOG"
case "$name" in
 sudo) exec "$@" ;;
 id) if [[ "$TEST_FAILURE" == root-runner ]]; then echo 0; else echo 1001; fi ;;
 dpkg-query)
  if [[ "$1" == -S ]]; then if [[ "$TEST_FAILURE" == wrong-owner ]]; then echo 'foreign: /usr/bin/bwrap'; else echo 'bubblewrap: /usr/bin/bwrap'; fi
  elif [[ "$*" == *bubblewrap ]]; then if [[ "$TEST_FAILURE" == wrong-package ]]; then echo 'wrong amd64 installed'; else echo '0.9.0-1ubuntu0.3 amd64 installed'; fi
  else echo 'apparmor fixture amd64'; fi ;;
 readlink) if [[ "$TEST_FAILURE" == wrong-path ]]; then echo /tmp/bwrap; else echo /usr/bin/bwrap; fi ;;
 stat) if [[ "$TEST_FAILURE" == setuid ]]; then echo 0:0:4755; else echo 0:0:755; fi ;;
 sha256sum)
  if [[ "$*" == '-c -' ]]; then read -r hash path; if [[ "$path" == *.deb && "$TEST_FAILURE" == archive-hash || "$path" == *extra-profiles* && "$TEST_FAILURE" == profile-hash ]]; then exit 31; fi
  else echo 'fixture-hash'; fi ;;
 uname) echo fixture-kernel ;;
 date) echo 2026-01-01T00:00:00+00:00 ;;
 apparmor_parser)
  case "$1" in
   --version) echo 'fixture-parser' ;;
   -Q) [[ "$TEST_FAILURE" != compile ]] || exit 32; if [[ "$TEST_FAILURE" == compile-warning ]]; then echo warning; fi ;;
   -a) [[ "$TEST_FAILURE" != load ]] || exit 33; if [[ "$TEST_FAILURE" == load-warning ]]; then echo warning; fi; printf 'loaded\n' > "$TEST_ROOT/loaded" ;;
   *) exit 81 ;;
  esac ;;
 cat)
  case "$1" in
   /sys/kernel/security/apparmor/profiles)
    if [[ -f "$TEST_ROOT/loaded" ]]; then echo 'bwrap (enforce)'; [[ "$TEST_FAILURE" == missing-enforce ]] || echo 'unpriv_bwrap (enforce)'
    elif [[ "$TEST_FAILURE" == existing-profile ]]; then echo 'bwrap (enforce)'; fi ;;
   /sys/*|/proc/*) echo fixture ;;
   *) exec /bin/cat "$@" ;;
  esac ;;
 findmnt) echo rw,nosuid,nodev,noexec ;;
 grep)
  if [[ "$1" == -R ]]; then [[ "$TEST_FAILURE" == competing-profile ]] && exit 0; exit 1; fi
  exec /usr/bin/grep "$@" ;;
 git) echo fixture-source ;;
 go)
  if [[ -f "$TEST_ROOT/loaded" ]]; then
   [[ "${ACR_CODEX_EXPECT_APPARMOR:-}" == 1 ]] || exit 82
   [[ "$TEST_FAILURE" != after-probe ]] || exit 34
   echo 'native child restriction passed'
  else echo 'bwrap: setting up uid map: Permission denied'; exit 1; fi ;;
 journalctl) echo 'apparmor="DENIED" comm="bwrap" fixture-probe' ;;
 apt-get) [[ "$*" == 'download apparmor-profiles=4.0.1really4.0.1-0ubuntu0.24.04.7' ]] || exit 81; [[ "$TEST_FAILURE" != download ]] || exit 35
  printf 'fixture archive\n' > apparmor-profiles_4.0.1really4.0.1-0ubuntu0.24.04.7_all.deb ;;
 dpkg-deb)
  if [[ "$1" == -f ]]; then
   [[ "$TEST_FAILURE" != package-metadata ]] || { echo wrong; exit 0; }
   printf 'Package: apparmor-profiles\nVersion: 4.0.1really4.0.1-0ubuntu0.24.04.7\nArchitecture: all\n'
  else [[ "$1" == -x ]] || exit 81; mkdir -p "$3/usr/share/apparmor/extra-profiles"; printf 'fixture profile\n' > "$3/usr/share/apparmor/extra-profiles/bwrap-userns-restrict"; fi ;;
 install) [[ "$1 $2 $3 $4 $5 $6" == '-o root -g root -m 0644' ]] || exit 81; cp "$7" "$8" ;;
 bwrap) exit 81 ;;
 *) exit 81 ;;
esac
`
			for _, name := range []string{"sudo", "id", "dpkg-query", "readlink", "stat", "sha256sum", "uname", "apparmor_parser", "cat", "findmnt", "grep", "git", "go", "journalctl", "apt-get", "dpkg-deb", "install", "bwrap", "date"} {
				writeWorkflowTestCommand(t, bin, name, mock)
			}
			command := exec.Command("/bin/bash", scriptPath)
			command.Dir = root
			runnerOS := "Linux"
			if failure == "wrong-platform" {
				runnerOS = "macOS"
			}
			log := filepath.Join(root, "commands.log")
			if err := os.WriteFile(log, nil, 0600); err != nil {
				t.Fatal(err)
			}
			command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + root, "RUNNER_TEMP=" + root, "RUNNER_OS=" + runnerOS, "RUNNER_ARCH=X64", "TEST_LOG=" + log, "TEST_FAILURE=" + failure, "TEST_ROOT=" + root}
			output, runErr := command.CombinedOutput()
			if (runErr == nil) != (failure == "success") {
				t.Fatalf("%s: %v\n%s", failure, runErr, output)
			}
			commands, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			ran := string(commands)
			stage := map[string]string{
				"root-runner": "id -u", "wrong-package": "dpkg-query -W", "wrong-path": "readlink -f", "wrong-owner": "dpkg-query -S", "setuid": "stat -c", "existing-profile": "grep -E", "competing-profile": "grep -R", "override": "findmnt", "download": "apt-get download", "archive-hash": "sha256sum -c -", "package-metadata": "dpkg-deb -f", "profile-hash": "dpkg-deb -x", "compile": "apparmor_parser -Q", "compile-warning": "apparmor_parser -Q", "load": "apparmor_parser -a", "load-warning": "apparmor_parser -a", "missing-enforce": "grep -Fx unpriv_bwrap", "after-probe": "go test -count=1",
			}[failure]
			if stage != "" && !strings.Contains(ran, stage) {
				t.Fatalf("failure never reached intended stage %s: %s\n%s", stage, ran, output)
			}

			passed := strings.Contains(string(output), "Scoped native boundary passed")
			if passed != (failure == "success") {
				t.Fatalf("invalid pass evidence: %s", output)
			}
			if failure == "success" {
				for _, required := range []string{"apt-get download apparmor-profiles=4.0.1really4.0.1-0ubuntu0.24.04.7", "apparmor_parser -Q -T", "apparmor_parser -a -T"} {
					if !strings.Contains(ran, required) {
						t.Fatalf("missing actual command %s: %s", required, ran)
					}
				}
				if strings.Count(ran, "go test -count=1") != 2 || strings.Contains(ran, "apt-get install") {
					t.Fatalf("unexpected probe/package execution: %s", ran)
				}
			}
			switch failure {
			case "wrong-platform", "root-runner", "wrong-package", "wrong-path", "wrong-owner", "setuid", "existing-profile", "competing-profile", "override", "download", "archive-hash", "package-metadata", "profile-hash":
				if strings.Contains(ran, "install -o") || strings.Contains(ran, "apparmor_parser -a") {
					t.Fatalf("unverified/conflicting profile changed host: %s", ran)
				}
			case "compile", "compile-warning":
				if strings.Contains(ran, "apparmor_parser -a") {
					t.Fatalf("invalid compile reached load: %s", ran)
				}
			}
		})
	}
}
