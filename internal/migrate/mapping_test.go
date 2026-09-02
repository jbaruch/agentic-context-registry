package migrate

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveMappingsUsesExplicitPrecedence(t *testing.T) {
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
