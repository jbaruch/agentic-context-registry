package migrate

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCliMapOverridesMappingFile(t *testing.T) {
	t.Parallel()

	packages := []PackageReport{{
		TesslIdentity: "example/alpha", Version: "1.2.3",
		PackageMapping: mappingGitHub, MappingCandidate: "github:manifest/alpha",
	}}
	file := []Mapping{{From: "example/alpha", Source: "github:file/alpha", Requested: "v1.2.3"}}
	cli := []Mapping{{From: "example/alpha", Source: "github:cli/alpha", Requested: "release-1"}}

	got, err := ResolveMappings(packages, file, cli)
	if err != nil {
		t.Fatal(err)
	}
	want := []Mapping{{
		From: "example/alpha", Source: "github:cli/alpha", Requested: "release-1",
		TesslVersion: "1.2.3", Origin: MappingOriginCLI,
		Overrides: []string{MappingOriginManifest, MappingOriginFile}, Explicit: true,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveMappings() = %#v, want %#v", got, want)
	}
}

func TestResolveMappingsNeverGuessesFromPackageName(t *testing.T) {
	t.Parallel()

	_, err := ResolveMappings([]PackageReport{{
		TesslIdentity: "example/orphan", Version: "latest", MappingCandidate: "github:example/orphan",
	}}, nil, nil)
	var unmapped *UnmappedPackageError
	if !errors.As(err, &unmapped) || unmapped.Package != "example/orphan" || unmapped.Candidate != "github:example/orphan" {
		t.Fatalf("ResolveMappings() error = %#v, want named unmapped package", err)
	}
}

func TestDecodeMappingFileRejectsWithinTierConflict(t *testing.T) {
	t.Parallel()

	_, err := DecodeMappingFile([]byte("schemaVersion: 1\npackages:\n  - from: example/alpha\n    source: github:one/alpha\n  - from: example/alpha\n    source: github:two/alpha\n"))
	var conflict *MappingConflictError
	if !errors.As(err, &conflict) || conflict.Origin != MappingOriginFile {
		t.Fatalf("DecodeMappingFile() error = %#v, want mapping-file conflict", err)
	}
}

func TestDecodeMappingFileAcceptsIdenticalDuplicates(t *testing.T) {
	t.Parallel()

	got, err := DecodeMappingFile([]byte("schemaVersion: 1\npackages:\n  - from: example/alpha\n    source: github:example/alpha\n  - from: example/alpha\n    source: github:example/alpha\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Origin != MappingOriginFile {
		t.Fatalf("DecodeMappingFile() = %#v", got)
	}
}

func TestParseInlineMapping(t *testing.T) {
	t.Parallel()

	got, err := ParseInlineMapping("tessl-labs/good-oss-citizen=github:tesslio/good-oss-citizen@v1.1.12")
	if err != nil {
		t.Fatal(err)
	}
	want := Mapping{From: "tessl-labs/good-oss-citizen", Source: "github:tesslio/good-oss-citizen", Requested: "v1.1.12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseInlineMapping() = %#v, want %#v", got, want)
	}
}

func TestDecodeMappingFileTypesOperatorSource(t *testing.T) {
	t.Parallel()

	_, err := DecodeMappingFile([]byte("schemaVersion: 1\npackages:\n  - from: example/alpha\n    source: github:Owner/Plugin\n"))
	var operator *OperatorSourceError
	if !errors.As(err, &operator) {
		t.Fatalf("DecodeMappingFile() error = %v, want OperatorSourceError", err)
	}
	const want = "invalid source \"github:Owner/Plugin\"; use github:owner/repository with lowercase canonical names"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestDecodeMappingFileRejectsUnknownFieldAndVersion(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"schemaVersion: 2\npackages: []\n",
		"schemaVersion: 1\nunknown: true\npackages: []\n",
	} {
		if _, err := DecodeMappingFile([]byte(content)); err == nil {
			t.Fatalf("DecodeMappingFile(%q) succeeded", content)
		}
	}
}

func TestMappingProducersBoundOnlyOperands(t *testing.T) {
	for _, input := range []struct{ name, value, token string }{
		{"controls", "bad\nvalue\x1b[2J\u202e", `"bad\nvalue\x1b[2J\u202e"`},
		{"long", strings.Repeat("x", 1024), `"` + strings.Repeat("x", 128) + `...[truncated]"`},
	} {
		t.Run(input.name, func(t *testing.T) {
			for _, test := range []struct{ value, want string }{
				{input.value, "invalid --map " + input.token + "; use FROM=github:owner/repository[@REQUESTED]"},
				{input.value + "=github:example/alpha", "invalid --map package " + input.token + "; use a workspace/package identity"},
				{"example/alpha=" + input.value, "invalid mapping source " + input.token + "; use github:owner/repository with lowercase canonical names"},
			} {
				_, err := ParseInlineMapping(test.value)
				if err == nil || err.Error() != test.want || strings.ContainsAny(err.Error(), "\n\x1b\u202e") {
					t.Fatalf("mapping error = %v, want %s", err, test.want)
				}
			}
			conflict := &MappingConflictError{From: input.value, Origin: MappingOriginFile}
			want := "mapping " + input.token + " is declared more than once with different values in mapping-file; keep one mapping for that package"
			if conflict.Error() != want {
				t.Fatalf("conflict = %s, want %s", conflict.Error(), want)
			}
		})
	}
	for _, test := range []struct{ name, content, suffix string }{
		{"unknown YAML field", "schemaVersion: 1\n" + strings.Repeat("x", 512) + ": true\n", "; use schemaVersion: 1 with a packages list"},
		{"trailing YAML", "schemaVersion: 1\npackages: []\n---\n[" + strings.Repeat("x", 512) + ":", "; keep one YAML document"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeMappingFile([]byte(test.content))
			if err == nil || !strings.HasSuffix(err.Error(), test.suffix) || len(err.Error()) > 700 || strings.ContainsAny(err.Error(), "\n\x1b\u202e") {
				t.Fatalf("YAML diagnostic must bound opaque detail before fixed guidance: %v", err)
			}
		})
	}
}
