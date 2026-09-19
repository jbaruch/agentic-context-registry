# Recorded Codex startup streams

Each `<version>/<platform>-startup.jsonl` is the complete stdout of one
unauthenticated `codex exec` run under ACR's exact argument vector, captured
from the official release binary inside ACR's OS read boundary. The stream
ends in the model service's `401 Unauthorized` refusal because no credential
was present; the events before `turn.started` are the startup prelude
`codexFinal` verifies, and the tail is the shape `codexUnauthorized`
recognizes. No account identifier, credential or credential fragment is
present; thread ids and request ids are kept.

| Version | Platform | Binary (official release asset, sha256) | Boundary | Captured (UTC) |
| --- | --- | --- | --- | --- |
| `codex-cli 0.153.2` | darwin/arm64, macOS 26.6.2 | `codex-aarch64-apple-darwin.tar.gz` `91dfc270f0dfbaec16d814f1aa90d4f27e74dc9e3784e64006bef3b79fe9e09c` | `/usr/bin/sandbox-exec` with `codexDarwinProfile` | 2026-09-18 |
| `codex-cli 0.154.0` | darwin/arm64, macOS 26.6.2 | `codex-aarch64-apple-darwin.tar.gz` `344310a0a591c1b192e04feff304321a69907c9498baaac331ca7e16ebcef9d7` | same | 2026-09-18 |
| `codex-cli 0.155.0` | darwin/arm64, macOS 26.6.2 | `codex-aarch64-apple-darwin.tar.gz` `5a584b7cddc2a97083cada53f10f5bc4231526b7f64105f6a5bb82d01ccdba49` | same | 2026-09-18 |
| `codex-cli 0.153.2` | linux/arm64, Debian 13 container, kernel 7.0.12-linuxkit, bubblewrap 0.12.0 | `codex-aarch64-unknown-linux-musl.tar.gz` `878693f9b370320ea21793f99ea1f5687b7d9aa1f2c733de693d9ec0baa4e62a` | `bwrap` with `codexLinuxView` | 2026-09-18 |
| `codex-cli 0.154.0` | linux/arm64, same container | `codex-aarch64-unknown-linux-musl.tar.gz` `583b48df32804213bdcd338c2e5adb06b34340821fa757a726cc0a524fa33c27` | same | 2026-09-18 |
| `codex-cli 0.155.0` | linux/arm64, same container | `codex-aarch64-unknown-linux-musl.tar.gz` `8b4a9c356916c515f7c93f918a01b8fa1371bcc9758addbfa723b85fbec5694b` | same | 2026-09-18 |

Argument vector: `codexArguments` with the runtime-derived feature switches
(every advertised feature outside the `removed` and `deprecated` stages
disabled, `skip_host_skill_discovery` enabled) and an isolated empty
`CODEX_HOME`; stdin was `Return exactly {"edits":[],"policyChanges":[]} and
nothing else.`. The Linux runs used the Docker relaxations
`--security-opt seccomp=unconfined --security-opt systempaths=unconfined` so
the container could create user namespaces. Hosted namespace behavior remains unverified until the real runner completes the lane.

Refresh a row when the verified-release table in `codex_runtime.go` gains a
release, or when `codexFinal` refuses a real release's prelude: re-record from
the official binary, never edit a stream to make a test green. A prelude that
no longer matches is a finding about ACR's startup contract.
