package tesslplugin

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Options configures producer conversion of one Tessl plugin package.
type Options struct {
	PackageRoot         string
	Repository          string
	AcceptAgentWidening bool
	DryRun              bool
}

// Convert maps Tessl plugin manifests onto agent-plugin.yaml.
func Convert(opts Options) (report Report, err error) {
	root, err := os.OpenRoot(opts.PackageRoot)
	if err != nil {
		return Report{}, fmt.Errorf("open package root %s: %w", opts.PackageRoot, err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close package root %s: %w", opts.PackageRoot, closeErr))
		}
	}()

	sources, err := Read(opts.PackageRoot)
	if err != nil {
		return Report{}, err
	}
	if err := checkAmbiguous(sources); err != nil {
		return Report{}, err
	}

	value, report, err := buildManifest(root, sources, opts)
	if err != nil {
		return Report{}, err
	}
	sortManifest(&value)
	if err := validateConverted(opts.PackageRoot, value); err != nil {
		return Report{}, err
	}
	published, err := publishedFromManifest(root, value)
	if err != nil {
		return Report{}, err
	}
	if err := rejectUnpublishable(published); err != nil {
		return Report{}, err
	}
	rendered, err := renderManifest(value)
	if err != nil {
		return Report{}, err
	}
	wrote, err := writeManifest(root, rendered, opts.DryRun)
	if err != nil {
		return Report{}, err
	}

	report.ReportVersion = reportVersion
	report.DryRun = opts.DryRun
	report.Wrote = wrote
	report.Manifest = manifest.Filename
	report.Artifacts = reportArtifacts(value)
	report.PublishedFiles = published
	sortReport(&report)
	return report, nil
}

func buildManifest(root *os.Root, sources Sources, opts Options) (manifest.Manifest, Report, error) {
	report := newReport()
	name, version, description, repository, homepage, license, author, private, rulesSpec, skillsSpec := identityFrom(sources)
	report.Package = name
	report.Version = version
	if sources.Plugin != nil {
		report.SourceManifest = pluginManifest
	} else {
		report.SourceManifest = tileManifest
	}

	if private != nil && *private {
		return manifest.Manifest{}, report, conversionError(CodeUnmappedField, "private",
			"private: true cannot be published through public GitHub releases; set private to false or drop it")
	}
	resolvedRepo, err := resolveRepository(repository, opts.Repository, name)
	if err != nil {
		return manifest.Manifest{}, report, err
	}
	appendProvenance(&report, homepage, license, author)

	rulePaths, err := expandRules(root, rulesSpec)
	if err != nil {
		return manifest.Manifest{}, report, err
	}
	skillPaths, err := expandSkills(root, skillsSpec)
	if err != nil {
		return manifest.Manifest{}, report, err
	}
	assignIDs(rulePaths, tilePathIndex(sources, false))
	assignIDs(skillPaths, tilePathIndex(sources, true))
	appendKeyLossy(&report, rulePaths, skillPaths)

	rules, lossy, notes, err := convertRules(root, rulePaths)
	if err != nil {
		return manifest.Manifest{}, report, err
	}
	report.Lossy = append(report.Lossy, lossy...)
	report.Notes = append(report.Notes, notes...)

	var plugin *PluginManifest
	if sources.Plugin != nil {
		plugin = sources.Plugin
	}
	mapped, wideningNotes, err := mapPluginHooks(plugin, opts.AcceptAgentWidening)
	if err != nil {
		return manifest.Manifest{}, report, err
	}
	for _, item := range wideningNotes {
		report.Notes = append(report.Notes, NoteItem{Path: item.Field, Reason: item.Reason})
	}

	ignored, tesslNotes, err := collectIgnoreAndNotes(root)
	if err != nil {
		return manifest.Manifest{}, report, err
	}
	report.Ignored = ignored
	report.Notes = append(report.Notes, tesslNotes...)

	value := manifest.Manifest{
		SchemaVersion: manifest.CurrentSchemaVersion,
		Name:          name,
		Version:       version,
		Description:   description,
		Source:        manifest.Source{Repository: resolvedRepo},
		Artifacts: manifest.Artifacts{
			Rules:  rules,
			Skills: convertSkills(skillPaths),
			Hooks:  assignHookIDs(mapped),
		},
	}
	return value, report, nil
}

func identityFrom(sources Sources) (name, version, description, repository, homepage, license string, author *Author, private *bool, rules, skills PathSpec) {
	if sources.Plugin != nil {
		plugin := sources.Plugin
		return plugin.Name, plugin.Version, plugin.Description, plugin.Repository, plugin.Homepage, plugin.License, plugin.Author, plugin.Private, plugin.Rules, plugin.Skills
	}
	tile := sources.Tile
	return tile.Name, tile.Version, tile.Summary, tile.Repository, tile.Homepage, tile.License, tile.Author, tile.Private, tile.Rules, tile.Skills
}

func resolveRepository(declared, flag, name string) (string, error) {
	repository := declared
	if repository == "" {
		repository = flag
	}
	if repository == "" {
		return "", conversionError(string(manifest.CodeRequired), "source.repository",
			"source.repository is required; pass --repository https://github.com/<owner>/<package> rather than guessing from %q", name)
	}
	want := "https://github.com/" + name
	if repository != want {
		return "", conversionError(string(manifest.CodeInvalidSource), "source.repository",
			"repository %q must equal %s; never synthesize it from the Tessl name", repository, want)
	}
	return repository, nil
}

func appendProvenance(report *Report, homepage, license string, author *Author) {
	if homepage != "" {
		report.Lossy = append(report.Lossy, LossyItem{Field: "homepage", Value: homepage, Reason: "provenance"})
	}
	if license != "" {
		report.Lossy = append(report.Lossy, LossyItem{Field: "license", Value: license, Reason: "provenance"})
	}
	if author == nil {
		return
	}
	if author.Name != "" {
		report.Lossy = append(report.Lossy, LossyItem{Field: "author.name", Value: author.Name, Reason: "provenance"})
	}
	if author.Email != "" {
		report.Lossy = append(report.Lossy, LossyItem{Field: "author.email", Value: author.Email, Reason: "provenance"})
	}
	if author.URL != "" {
		report.Lossy = append(report.Lossy, LossyItem{Field: "author.url", Value: author.URL, Reason: "provenance"})
	}
}

func tilePathIndex(sources Sources, skills bool) map[string]string {
	if sources.Tile == nil {
		return nil
	}
	spec := sources.Tile.Rules
	if skills {
		spec = sources.Tile.Skills
	}
	index := make(map[string]string, len(spec.Named)+len(spec.List))
	for _, named := range spec.Named {
		relative := named.Path
		if skills {
			relative = skillDirectory(relative)
		}
		index[relative] = named.ID
	}
	return index
}

func assignIDs(paths []NamedPath, tileKeys map[string]string) {
	for index, named := range paths {
		if named.ID != "" {
			continue
		}
		if id, ok := tileKeys[named.Path]; ok {
			paths[index].ID = id
			continue
		}
		paths[index].ID = basenameID(named.Path)
	}
}

func appendKeyLossy(report *Report, rules, skills []NamedPath) {
	for _, named := range rules {
		if base := basenameID(named.Path); named.ID != "" && named.ID != base {
			report.Lossy = append(report.Lossy, LossyItem{ID: named.ID, Kind: "rule", Field: named.Path, Value: named.ID, Reason: "tile-key"})
		}
	}
	for _, named := range skills {
		if base := basenameID(named.Path); named.ID != "" && named.ID != base {
			report.Lossy = append(report.Lossy, LossyItem{ID: named.ID, Kind: "skill", Field: named.Path, Value: named.ID, Reason: "tile-key"})
		}
	}
}

func convertRules(root *os.Root, paths []NamedPath) ([]manifest.RuleArtifact, []LossyItem, []NoteItem, error) {
	rules := make([]manifest.RuleArtifact, 0, len(paths))
	var lossy []LossyItem
	var notes []NoteItem
	for _, named := range paths {
		content, err := root.ReadFile(named.Path)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read %s: %w", named.Path, err)
		}
		activation, err := activationFromRuleFile(named.Path, content)
		if err != nil {
			return nil, nil, nil, err
		}
		for index := range activation.Lossy {
			activation.Lossy[index].ID = named.ID
			activation.Lossy[index].Kind = "rule"
		}
		lossy = append(lossy, activation.Lossy...)
		for _, pattern := range activation.Activation.Paths {
			if tesslOnlyGlob(pattern) {
				notes = append(notes, NoteItem{Path: named.Path, Reason: "activation-glob-tessl-only:" + pattern})
			}
		}
		rules = append(rules, manifest.RuleArtifact{ID: named.ID, Path: named.Path, Activation: activation.Activation})
	}
	return rules, lossy, notes, nil
}

func convertSkills(paths []NamedPath) []manifest.SkillArtifact {
	skills := make([]manifest.SkillArtifact, 0, len(paths))
	for _, named := range paths {
		skills = append(skills, manifest.SkillArtifact{ID: named.ID, Path: named.Path})
	}
	return skills
}

func assignHookIDs(hooks []mappedHook) []manifest.HookArtifact {
	counts := make(map[string]int, len(hooks))
	for _, hook := range hooks {
		counts[hookBasename(hook.Path)]++
	}
	result := make([]manifest.HookArtifact, 0, len(hooks))
	for _, hook := range hooks {
		id := hookBasename(hook.Path)
		if counts[id] > 1 {
			id = id + "-" + string(hook.Event)
		}
		args := hook.Args
		if len(args) == 0 {
			args = nil
		}
		result = append(result, manifest.HookArtifact{ID: id, Event: hook.Event, Path: hook.Path, Args: args})
	}
	return result
}

func tesslOnlyGlob(pattern string) bool {
	switch pattern {
	case ".tessl-plugin/plugin.json", ".tesslignore", ".tileignore", "tile.json":
		return true
	default:
		return false
	}
}

func collectIgnoreAndNotes(root *os.Root) ([]IgnoredItem, []NoteItem, error) {
	var ignored []IgnoredItem
	var notes []NoteItem
	tesslLines, err := readIgnoreLines(root, ".tesslignore")
	if err != nil {
		return nil, nil, err
	}
	for _, line := range tesslLines {
		ignored = append(ignored, IgnoredItem{Path: line, Reason: "tesslignore"})
	}
	tileLines, err := readIgnoreLines(root, ".tileignore")
	if err != nil {
		return nil, nil, err
	}
	for _, line := range tileLines {
		ignored = append(ignored, IgnoredItem{Path: line, Reason: "tileignore"})
	}
	present, err := rootFileExists(root, "tessl-package.json")
	if err != nil {
		return nil, nil, err
	}
	if present {
		notes = append(notes, NoteItem{Path: "tessl-package.json", Reason: "install-time-state"})
	}
	return ignored, notes, nil
}

func readIgnoreLines(root *os.Root, relative string) ([]string, error) {
	data, err := root.ReadFile(relative)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", relative, err)
	}
	var lines []string
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func rootFileExists(root *os.Root, relative string) (bool, error) {
	_, err := root.ReadFile(relative)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", relative, err)
	}
	return true, nil
}

func publishedFromManifest(root *os.Root, value manifest.Manifest) ([]string, error) {
	files := map[string]struct{}{manifest.Filename: {}}
	for _, rule := range value.Artifacts.Rules {
		files[rule.Path] = struct{}{}
	}
	for _, script := range value.Artifacts.Scripts {
		files[script.Path] = struct{}{}
	}
	for _, hook := range value.Artifacts.Hooks {
		files[hook.Path] = struct{}{}
	}
	for _, skill := range value.Artifacts.Skills {
		err := fs.WalkDir(root.FS(), skill.Path, func(relative string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("skill %q contains a symbolic link %q; replace it with a regular file or directory", skill.Path, relative)
			}
			if entry.IsDir() {
				return nil
			}
			files[relative] = struct{}{}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(files))
	for file := range files {
		result = append(result, file)
	}
	sort.Strings(result)
	return result, nil
}

func rejectUnpublishable(files []string) error {
	for _, relative := range files {
		for _, segment := range strings.Split(relative, "/") {
			switch segment {
			case "__pycache__", "node_modules", ".git", ".DS_Store":
				return conversionError(CodeUnpublishableContent, relative,
					"published path %s contains unpublishable segment %q; remove it from the skill tree before converting", relative, segment)
			}
		}
		if strings.HasSuffix(relative, ".pyc") || strings.HasSuffix(relative, ".pyo") {
			return conversionError(CodeUnpublishableContent, relative,
				"published path %s is bytecode; remove it from the skill tree before converting", relative)
		}
	}
	return nil
}

func validateConverted(packageRoot string, value manifest.Manifest) error {
	err := manifest.Validate(packageRoot, value)
	if err == nil {
		return nil
	}
	var issues *manifest.ValidationErrors
	if !errors.As(err, &issues) {
		return err
	}
	filtered := make([]manifest.ValidationError, 0, len(issues.Issues))
	for _, issue := range issues.Issues {
		if issue.Field == manifest.Filename && issue.Code == manifest.CodePathNotFound {
			continue
		}
		filtered = append(filtered, issue)
	}
	if len(filtered) == 0 {
		return nil
	}
	return wrapValidation(&manifest.ValidationErrors{Issues: filtered})
}

func wrapValidation(err error) error {
	var issues *manifest.ValidationErrors
	if errors.As(err, &issues) && len(issues.Issues) != 0 {
		first := issues.Issues[0]
		return conversionError(string(first.Code), first.Field, "%s", issues.Error())
	}
	return err
}

func checkAmbiguous(sources Sources) error {
	if sources.Plugin == nil || sources.Tile == nil {
		return nil
	}
	plugin, tile := sources.Plugin, sources.Tile
	if plugin.Name != "" && tile.Name != "" && plugin.Name != tile.Name {
		return conversionError(CodeAmbiguousManifest, "name",
			"plugin.json name %q disagrees with tile.json name %q; make them match or keep one manifest", plugin.Name, tile.Name)
	}
	if plugin.Version != "" && tile.Version != "" && plugin.Version != tile.Version {
		return conversionError(CodeAmbiguousManifest, "version",
			"plugin.json version %q disagrees with tile.json version %q; make them match or keep one manifest", plugin.Version, tile.Version)
	}
	if plugin.Description != "" && tile.Summary != "" && plugin.Description != tile.Summary {
		return conversionError(CodeAmbiguousManifest, "description",
			"plugin.json description disagrees with tile.json summary; make them match or keep one manifest")
	}
	if plugin.Repository != "" && tile.Repository != "" && plugin.Repository != tile.Repository {
		return conversionError(CodeAmbiguousManifest, "repository",
			"plugin.json repository disagrees with tile.json repository; make them match or keep one manifest")
	}
	if err := comparePathSets("rules", declaredRulePaths(plugin.Rules), declaredRulePaths(tile.Rules)); err != nil {
		return err
	}
	if err := comparePathSets("skills", declaredSkillPaths(plugin.Skills), declaredSkillPaths(tile.Skills)); err != nil {
		return err
	}
	return nil
}

func declaredRulePaths(spec PathSpec) []string {
	switch spec.Kind {
	case PathSpecList:
		return append([]string(nil), spec.List...)
	case PathSpecNamed:
		paths := make([]string, 0, len(spec.Named))
		for _, named := range spec.Named {
			paths = append(paths, named.Path)
		}
		return paths
	default:
		return nil
	}
}

func declaredSkillPaths(spec PathSpec) []string {
	switch spec.Kind {
	case PathSpecList:
		return append([]string(nil), spec.List...)
	case PathSpecNamed:
		paths := make([]string, 0, len(spec.Named))
		for _, named := range spec.Named {
			paths = append(paths, skillDirectory(named.Path))
		}
		return paths
	default:
		return nil
	}
}

func comparePathSets(field string, pluginPaths, tilePaths []string) error {
	if len(pluginPaths) == 0 || len(tilePaths) == 0 {
		return nil
	}
	pluginSet := make(map[string]struct{}, len(pluginPaths))
	for _, relative := range pluginPaths {
		pluginSet[relative] = struct{}{}
	}
	tileSet := make(map[string]struct{}, len(tilePaths))
	for _, relative := range tilePaths {
		tileSet[relative] = struct{}{}
	}
	if len(pluginSet) != len(tileSet) {
		return conversionError(CodeAmbiguousManifest, field,
			"plugin.json %s paths disagree with tile.json; make them match or keep one manifest", field)
	}
	for relative := range pluginSet {
		if _, ok := tileSet[relative]; !ok {
			return conversionError(CodeAmbiguousManifest, field,
				"plugin.json %s path %q is missing from tile.json; make them match or keep one manifest", field, relative)
		}
	}
	return nil
}
