// Package manifest defines and validates the agent-plugin.yaml package contract.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	// Filename is the package manifest name at the root of every plugin.
	Filename = "agent-plugin.yaml"
	// CurrentSchemaVersion is the only schema version this package understands.
	CurrentSchemaVersion = 1
)

// ErrorCode identifies a machine-readable manifest validation failure.
type ErrorCode string

const (
	CodeRequired                ErrorCode = "required"
	CodeUnsupportedSchema       ErrorCode = "unsupported_schema_version"
	CodeInvalidPackageName      ErrorCode = "invalid_package_name"
	CodeInvalidVersion          ErrorCode = "invalid_version"
	CodeInvalidSource           ErrorCode = "invalid_source"
	CodeNoArtifacts             ErrorCode = "no_artifacts"
	CodeInvalidArtifactID       ErrorCode = "invalid_artifact_id"
	CodeDuplicateArtifactID     ErrorCode = "duplicate_artifact_id"
	CodeInvalidPath             ErrorCode = "invalid_path"
	CodePathNotFound            ErrorCode = "path_not_found"
	CodeInvalidArtifactType     ErrorCode = "invalid_artifact_type"
	CodeInvalidSkillTree        ErrorCode = "invalid_skill_tree"
	CodeInvalidRuleActivation   ErrorCode = "invalid_rule_activation"
	CodeUnsupportedHookEvent    ErrorCode = "unsupported_hook_event"
	CodeDuplicateActivationPath ErrorCode = "duplicate_activation_path"
)

// HookEvent is an agent-neutral lifecycle event. Adapters own native naming.
type HookEvent string

const (
	HookSessionStart     HookEvent = "session-start"
	HookSessionEnd       HookEvent = "session-end"
	HookUserPromptSubmit HookEvent = "user-prompt-submit"
	HookPreToolUse       HookEvent = "pre-tool-use"
	HookPostToolUse      HookEvent = "post-tool-use"
	HookStop             HookEvent = "stop"
)

// ActivationMode controls when an agent should load a rule.
type ActivationMode string

const (
	ActivationAlways ActivationMode = "always"
	ActivationPaths  ActivationMode = "paths"
)

// Manifest is the versioned, agent-neutral package description.
type Manifest struct {
	SchemaVersion int       `json:"schemaVersion" yaml:"schemaVersion"`
	Name          string    `json:"name" yaml:"name"`
	Version       string    `json:"version" yaml:"version"`
	Description   string    `json:"description,omitempty" yaml:"description,omitempty"`
	Source        Source    `json:"source" yaml:"source"`
	Artifacts     Artifacts `json:"artifacts" yaml:"artifacts"`
}

// Source identifies the GitHub repository that owns a package.
type Source struct {
	Repository string `json:"repository" yaml:"repository"`
}

// Artifacts groups logical package content by adapter-neutral class.
type Artifacts struct {
	Rules   []RuleArtifact   `json:"rules,omitempty" yaml:"rules,omitempty"`
	Skills  []SkillArtifact  `json:"skills,omitempty" yaml:"skills,omitempty"`
	Scripts []ScriptArtifact `json:"scripts,omitempty" yaml:"scripts,omitempty"`
	Hooks   []HookArtifact   `json:"hooks,omitempty" yaml:"hooks,omitempty"`
}

// RuleArtifact is a Markdown instruction and its activation policy.
type RuleArtifact struct {
	ID         string         `json:"id" yaml:"id"`
	Path       string         `json:"path" yaml:"path"`
	Activation RuleActivation `json:"activation" yaml:"activation"`
}

// RuleActivation describes either unconditional or path-based activation.
type RuleActivation struct {
	Mode  ActivationMode `json:"mode" yaml:"mode"`
	Paths []string       `json:"paths,omitempty" yaml:"paths,omitempty"`
}

// SkillArtifact names a complete skill directory.
type SkillArtifact struct {
	ID   string `json:"id" yaml:"id"`
	Path string `json:"path" yaml:"path"`
}

// ScriptArtifact names a standalone executable or helper file.
type ScriptArtifact struct {
	ID   string `json:"id" yaml:"id"`
	Path string `json:"path" yaml:"path"`
}

// HookArtifact binds an entrypoint file to an agent-neutral event.
type HookArtifact struct {
	ID    string    `json:"id" yaml:"id"`
	Event HookEvent `json:"event" yaml:"event"`
	Path  string    `json:"path" yaml:"path"`
	Args  []string  `json:"args,omitempty" yaml:"args,omitempty"`
}

// ValidationError is one actionable failure at a stable manifest field.
type ValidationError struct {
	Code    ErrorCode
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Field, e.Message)
}

// ValidationErrors preserves validation failures in deterministic field order.
type ValidationErrors struct {
	Issues []ValidationError
}

func (e *ValidationErrors) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		parts = append(parts, issue.Error())
	}
	return "manifest validation failed: " + strings.Join(parts, "; ")
}

// Has reports whether the result contains a specific machine-readable code.
func (e *ValidationErrors) Has(code ErrorCode) bool {
	for _, issue := range e.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

// Load decodes and validates the package rooted at root.
func Load(root string) (Manifest, error) {
	manifestPath := filepath.Join(root, Filename)
	info, err := os.Lstat(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, fmt.Errorf("%s not found: add %s at the package root: %w", manifestPath, Filename, err)
		}
		return Manifest{}, fmt.Errorf("inspect %s: %w", manifestPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Manifest{}, &ValidationErrors{Issues: []ValidationError{{
			Code:    CodeInvalidArtifactType,
			Field:   Filename,
			Message: fmt.Sprintf("%q contains a symbolic link; package artifacts must be regular files or directories", Filename),
		}}}
	}
	if !info.Mode().IsRegular() {
		return Manifest{}, &ValidationErrors{Issues: []ValidationError{{
			Code:    CodeInvalidArtifactType,
			Field:   Filename,
			Message: fmt.Sprintf("%q must be a regular file", Filename),
		}}}
	}

	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", manifestPath, err)
	}

	var header struct {
		SchemaVersion *int `yaml:"schemaVersion"`
	}
	if err := yaml.Unmarshal(contents, &header); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", manifestPath, err)
	}
	if header.SchemaVersion == nil {
		return Manifest{}, &ValidationErrors{Issues: []ValidationError{{
			Code:    CodeRequired,
			Field:   "schemaVersion",
			Message: fmt.Sprintf("set schemaVersion to %d", CurrentSchemaVersion),
		}}}
	}
	if *header.SchemaVersion != CurrentSchemaVersion {
		return Manifest{}, unsupportedSchemaVersion(*header.SchemaVersion)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)

	var result Manifest
	if err := decoder.Decode(&result); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", manifestPath, err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return Manifest{}, fmt.Errorf("decode trailing content in %s: %w", manifestPath, err)
		}
		return Manifest{}, fmt.Errorf("decode %s: multiple YAML documents are not supported", manifestPath)
	}

	if err := Validate(root, result); err != nil {
		return Manifest{}, err
	}
	return result, nil
}

var (
	packageNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)
	artifactIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
)

// Validate checks manifest semantics and every referenced package path.
func Validate(root string, value Manifest) error {
	problems := &ValidationErrors{}
	add := func(code ErrorCode, field, message string) {
		problems.Issues = append(problems.Issues, ValidationError{Code: code, Field: field, Message: message})
	}

	if value.SchemaVersion != CurrentSchemaVersion {
		return unsupportedSchemaVersion(value.SchemaVersion)
	}

	validateFilesystemPath(root, Filename, Filename, false, add)

	validPackageName := packageNamePattern.MatchString(value.Name)
	if !validPackageName {
		add(CodeInvalidPackageName, "name", "use a lowercase owner/package identity")
	}
	if !isSemver(value.Version) {
		add(CodeInvalidVersion, "version", "use a valid semantic version such as 1.2.3")
	}
	validateSource(value, validPackageName, add)

	artifactCount := len(value.Artifacts.Rules) + len(value.Artifacts.Skills) + len(value.Artifacts.Scripts) + len(value.Artifacts.Hooks)
	if artifactCount == 0 {
		add(CodeNoArtifacts, "artifacts", "declare at least one rule, skill, script, or hook")
	}

	seenIDs := make(map[string]string, artifactCount)
	validateID := func(id, field string) {
		if !artifactIDPattern.MatchString(id) {
			add(CodeInvalidArtifactID, field, "use a lowercase kebab-case artifact ID")
			return
		}
		if firstField, exists := seenIDs[id]; exists {
			add(CodeDuplicateArtifactID, field, fmt.Sprintf("artifact ID %q is already declared at %s", id, firstField))
			return
		}
		seenIDs[id] = field
	}

	for index, rule := range value.Artifacts.Rules {
		field := fmt.Sprintf("artifacts.rules[%d]", index)
		validateID(rule.ID, field+".id")
		validateRuleActivation(rule.Activation, field+".activation", add)
		if validateRelativePath(rule.Path, field+".path", add) {
			validateFilesystemPath(root, rule.Path, field+".path", false, add)
		}
	}

	for index, skill := range value.Artifacts.Skills {
		field := fmt.Sprintf("artifacts.skills[%d]", index)
		validateID(skill.ID, field+".id")
		if !validateRelativePath(skill.Path, field+".path", add) {
			continue
		}
		if !validateFilesystemPath(root, skill.Path, field+".path", true, add) {
			continue
		}
		validateFilesystemPath(root, path.Join(skill.Path, "SKILL.md"), field+".path", false, add)
		if _, err := collectSkillFiles(root, skill.Path); err != nil {
			add(CodeInvalidSkillTree, field+".path", err.Error())
		}
	}

	for index, script := range value.Artifacts.Scripts {
		field := fmt.Sprintf("artifacts.scripts[%d]", index)
		validateID(script.ID, field+".id")
		if validateRelativePath(script.Path, field+".path", add) {
			validateFilesystemPath(root, script.Path, field+".path", false, add)
		}
	}

	supportedEvents := map[HookEvent]struct{}{
		HookSessionStart:     {},
		HookSessionEnd:       {},
		HookUserPromptSubmit: {},
		HookPreToolUse:       {},
		HookPostToolUse:      {},
		HookStop:             {},
	}
	for index, hook := range value.Artifacts.Hooks {
		field := fmt.Sprintf("artifacts.hooks[%d]", index)
		validateID(hook.ID, field+".id")
		if _, supported := supportedEvents[hook.Event]; !supported {
			add(CodeUnsupportedHookEvent, field+".event", fmt.Sprintf("event %q is not in the v1 hook vocabulary", hook.Event))
		}
		if validateRelativePath(hook.Path, field+".path", add) {
			validateFilesystemPath(root, hook.Path, field+".path", false, add)
		}
	}

	if len(problems.Issues) != 0 {
		return problems
	}
	return nil
}

func validateSource(value Manifest, validPackageName bool, add func(ErrorCode, string, string)) {
	if value.Source.Repository == "" {
		add(CodeRequired, "source.repository", "set the canonical GitHub repository URL")
		return
	}
	parsed, err := url.Parse(value.Source.Repository)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" {
		add(CodeInvalidSource, "source.repository", "use a canonical URL such as https://github.com/owner/package")
		return
	}
	if !validPackageName {
		return
	}
	want := "https://github.com/" + value.Name
	if value.Source.Repository != want {
		add(CodeInvalidSource, "source.repository", fmt.Sprintf("repository must match package identity exactly: %s", want))
	}
}

func validateRuleActivation(value RuleActivation, field string, add func(ErrorCode, string, string)) {
	switch value.Mode {
	case ActivationAlways:
		if len(value.Paths) != 0 {
			add(CodeInvalidRuleActivation, field+".paths", "omit paths when activation mode is always")
		}
	case ActivationPaths:
		if len(value.Paths) == 0 {
			add(CodeInvalidRuleActivation, field+".paths", "declare at least one glob when activation mode is paths")
		}
	default:
		add(CodeInvalidRuleActivation, field+".mode", "use activation mode always or paths")
	}

	seen := make(map[string]struct{}, len(value.Paths))
	for index, pattern := range value.Paths {
		patternField := fmt.Sprintf("%s.paths[%d]", field, index)
		if pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "\\") || containsParentSegment(pattern) {
			add(CodeInvalidRuleActivation, patternField, "use a non-empty, package-relative POSIX glob without parent traversal")
		}
		if _, exists := seen[pattern]; exists {
			add(CodeDuplicateActivationPath, patternField, fmt.Sprintf("glob %q is already declared", pattern))
		}
		seen[pattern] = struct{}{}
	}
}

func validateRelativePath(value, field string, add func(ErrorCode, string, string)) bool {
	if value == "" {
		add(CodeRequired, field, "set a package-relative path")
		return false
	}
	if value == "." || strings.HasPrefix(value, "/") || hasWindowsVolumePrefix(value) || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') || path.Clean(value) != value || containsParentSegment(value) {
		add(CodeInvalidPath, field, "use a normalized package-relative POSIX path without parent traversal")
		return false
	}
	return true
}

func hasWindowsVolumePrefix(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	letter := value[0]
	return letter >= 'A' && letter <= 'Z' || letter >= 'a' && letter <= 'z'
}

func containsParentSegment(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func validateFilesystemPath(root, relative, field string, wantDirectory bool, add func(ErrorCode, string, string)) bool {
	current := filepath.Clean(root)
	segments := strings.Split(relative, "/")
	for index, segment := range segments {
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				add(CodePathNotFound, field, fmt.Sprintf("%q does not exist in the package", relative))
			} else {
				add(CodeInvalidArtifactType, field, fmt.Sprintf("inspect %q: %v", relative, err))
			}
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			add(CodeInvalidArtifactType, field, fmt.Sprintf("%q contains a symbolic link; package artifacts must be regular files or directories", relative))
			return false
		}
		if index < len(segments)-1 && !info.IsDir() {
			add(CodeInvalidArtifactType, field, fmt.Sprintf("%q has a non-directory parent", relative))
			return false
		}
		if index == len(segments)-1 {
			if wantDirectory && !info.IsDir() {
				add(CodeInvalidArtifactType, field, fmt.Sprintf("%q must be a directory", relative))
				return false
			}
			if !wantDirectory && !info.Mode().IsRegular() {
				add(CodeInvalidArtifactType, field, fmt.Sprintf("%q must be a regular file", relative))
				return false
			}
		}
	}
	return true
}

func isSemver(value string) bool {
	coreAndPre := value
	if plus := strings.IndexByte(value, '+'); plus >= 0 {
		if plus == len(value)-1 || strings.Contains(value[plus+1:], "+") || !validIdentifiers(value[plus+1:], false) {
			return false
		}
		coreAndPre = value[:plus]
	}

	core := coreAndPre
	if dash := strings.IndexByte(coreAndPre, '-'); dash >= 0 {
		if dash == len(coreAndPre)-1 || !validIdentifiers(coreAndPre[dash+1:], true) {
			return false
		}
		core = coreAndPre[:dash]
	}

	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !validNumericIdentifier(part) {
			return false
		}
	}
	return true
}

func validIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, character := range identifier {
			if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' {
				return false
			}
			if character < '0' || character > '9' {
				numeric = false
			}
		}
		if rejectNumericLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func validNumericIdentifier(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func unsupportedSchemaVersion(version int) *ValidationErrors {
	return &ValidationErrors{Issues: []ValidationError{{
		Code:    CodeUnsupportedSchema,
		Field:   "schemaVersion",
		Message: fmt.Sprintf("version %d is not supported; use schemaVersion %d", version, CurrentSchemaVersion),
	}}}
}
