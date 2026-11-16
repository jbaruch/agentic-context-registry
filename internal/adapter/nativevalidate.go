package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/realize"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// ValidateFileProjection checks planned whole-file targets, executable modes,
// and complete regular-only native skill trees before native config semantics
// are inspected by an adapter. A __pycache__ directory of only regular *.pyc
// files is disposable interpreter output and is not an unexpected entry.
func ValidateFileProjection(request ValidateRequest, skillPrefix string) error {
	candidates := make(map[string]CandidateFile)
	for _, file := range request.Files {
		if prior, exists := candidates[file.Path]; exists {
			if prior.Mode.Perm() != file.Mode.Perm() || prior.Ownership != file.Ownership || !bytes.Equal(prior.Content, file.Content) {
				return NativeError(CodeInvalidSkillTree, "candidate %q occurs with inconsistent projections", file.Path)
			}
			continue
		}
		candidates[file.Path] = file
	}

	expectedSkills := make(map[string]map[string]fs.FileMode)
	for _, item := range request.Plan.Items {
		if item.Kind != OutputGeneratedFile {
			continue
		}
		candidate, exists := candidates[item.Target]
		if !exists {
			code := CodeInvalidExecutableMode
			if item.Owner.Kind == ArtifactSkill {
				code = CodeInvalidSkillTree
			}
			return NativeError(code, "planned file %q is missing from the compiled candidate set", item.Target)
		}
		if candidate.Mode.Perm() != item.Mode.Perm() {
			code := CodeInvalidExecutableMode
			if item.Owner.Kind == ArtifactSkill && item.Mode.Perm() != 0o755 {
				code = CodeInvalidSkillTree
			}
			return NativeError(code, "file %q has mode %04o, want %04o", item.Target, candidate.Mode.Perm(), item.Mode.Perm())
		}
		if item.Owner.Kind != ArtifactSkill {
			continue
		}
		root, ok := nativeSkillRoot(item.Target, skillPrefix)
		if !ok {
			return NativeError(CodeInvalidSkillTree, "skill target %q is outside %q", item.Target, skillPrefix)
		}
		if expectedSkills[root] == nil {
			expectedSkills[root] = make(map[string]fs.FileMode)
		}
		expectedSkills[root][item.Target] = item.Mode.Perm()
	}

	// Historical whole-file leaves reach the planner, which alone decides
	// whether their current bytes and mode authorize removal.
	previousFiles := make(map[string]bool)
	for _, target := range request.Previous.Targets {
		if target.Ownership == realize.OwnershipGenerated && len(target.Entries) == 1 &&
			target.Entries[0].ArtifactKind == realize.ArtifactFile && target.Entries[0].Adapter == request.Plan.Adapter.ID {
			previousFiles[target.Path] = true
		}
	}

	for root, expected := range expectedSkills {
		if _, exists := expected[path.Join(root, "SKILL.md")]; !exists {
			return NativeError(CodeInvalidSkillTree, "skill tree %q has no SKILL.md", root)
		}
		if request.Project == nil {
			continue
		}
		entries, err := WalkSnapshot(request.Project, root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return NativeError(CodeInvalidSkillTree, "inspect skill tree %q: %v", root, err)
		}
		caches := pureBytecodeCaches(entries)
		for _, entry := range entries {
			if entry.Mode&fs.ModeSymlink != 0 || !entry.Mode.IsRegular() && !entry.Mode.IsDir() {
				return NativeError(CodeInvalidSkillTree, "skill tree %q contains symlink or special entry %q", root, entry.Path)
			}
			if caches[entry.Path] || caches[path.Dir(entry.Path)] {
				continue
			}
			if entry.Mode.IsDir() {
				continue
			}
			if _, exists := expected[entry.Path]; !exists && !previousFiles[entry.Path] {
				return NativeError(CodeInvalidSkillTree, "skill tree %q contains unexpected file %q", root, entry.Path)
			}
		}
	}
	return nil
}

// pureBytecodeCaches classifies whole directories so declared non-bytecode files
// cannot hide unexpected bytecode siblings.
func pureBytecodeCaches(entries []ObservedEntry) map[string]bool {
	caches := make(map[string]bool)
	for _, entry := range entries {
		if entry.Mode.IsDir() && path.Base(entry.Path) == "__pycache__" {
			caches[entry.Path] = true
		}
	}
	for _, entry := range entries {
		parent := path.Dir(entry.Path)
		if caches[parent] && (!entry.Mode.IsRegular() || !strings.HasSuffix(entry.Path, ".pyc")) {
			caches[parent] = false
		}
	}
	return caches
}

func nativeSkillRoot(target, prefix string) (string, bool) {
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	if !strings.HasPrefix(target, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(target, prefix)
	name, _, found := strings.Cut(remainder, "/")
	if !found || name == "" {
		return "", false
	}
	return prefix + name, true
}

// ValidateUniqueJSONMembers rejects repeated decoded object members at every
// depth while accepting arbitrary valid JSON value shapes.
func ValidateUniqueJSONMembers(content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := validateJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("decode trailing JSON: %w", err)
		}
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object member is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return NativeError(CodeDuplicateConfigEntry, "JSON object member %q occurs more than once", key)
			}
			seen[key] = struct{}{}
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

// DecodeTOML decodes one complete TOML document. The parser rejects repeated
// fully qualified keys and malformed table structure.
func DecodeTOML(content []byte, target any) error {
	return toml.Unmarshal(content, target)
}

// IsTOMLDuplicateDefinition reports whether a typed TOML decode failure came
// from a structurally valid document that the generic decoder rejected. With
// no destination-schema constraints, such failures are duplicate or
// conflicting TOML definitions rather than adapter shape mismatches.
func IsTOMLDuplicateDefinition(content []byte, decodeErr error) bool {
	var typed *toml.DecodeError
	if !errors.As(decodeErr, &typed) {
		return false
	}
	var generic map[string]any
	genericErr := toml.Unmarshal(content, &generic)
	if !errors.As(genericErr, &typed) {
		return false
	}
	parser := unstable.Parser{}
	parser.Reset(content)
	for parser.NextExpression() {
	}
	return parser.Error() == nil
}

// UniquePlanOwnerKeys rejects duplicate desired structural ownership keys.
func UniquePlanOwnerKeys(plan NativePlan, target string, nativeEvent func(OwnerRef) (string, bool)) error {
	var keys []string
	for _, item := range plan.Items {
		if item.Target != target || item.Kind != OutputConfigMerge {
			continue
		}
		event, ok := nativeEvent(item.Owner)
		if !ok {
			return NativeError(CodeInvalidNativeEvent, "hook %q has unsupported event %q", item.Owner.ArtifactID, item.Owner.Event)
		}
		keys = append(keys, CanonicalConfigOwnerKey(item.Owner, plan.Adapter.ID, target, event))
	}
	sort.Strings(keys)
	for index := 1; index < len(keys); index++ {
		if keys[index-1] == keys[index] {
			return NativeError(CodeDuplicateConfigEntry, "desired ownership key %q occurs more than once", keys[index])
		}
	}
	return nil
}
