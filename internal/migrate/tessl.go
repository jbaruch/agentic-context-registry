package migrate

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

const (
	tesslJSONPath     = "tessl.json"
	tesslPluginsRoot  = ".tessl/plugins"
	pluginManifestRel = ".tessl-plugin/plugin.json"
	tileManifestName  = "tile.json"
	pluginManifest    = "plugin.json"
	tileManifest      = "tile.json"

	mappingGitHub   = "github-mapped"
	mappingUnmapped = "unmapped"
)

// PackageInstall is one installed Tessl plugin after both manifest shapes are
// read and compared. plugin.json is authoritative when both exist.
type PackageInstall struct {
	TesslIdentity    string
	Name             string
	Version          string
	ManifestKind     string
	PackageMapping   string
	MappingCandidate string
	Root             string
	Rules            []DeclaredPath
	Skills           []DeclaredPath
	Hooks            []DeclaredHook
}

// DeclaredPath is a rule file or skill directory declared by a Tessl manifest.
type DeclaredPath struct {
	ID         string
	Path       string
	Ambiguous  bool
	FromPlugin bool
	FromTile   bool
}

// DeclaredHook is one hook command taken from plugin.json. tile.json cannot
// express hooks, so tile silence is not a disagreement.
type DeclaredHook struct {
	ID          string
	NativeEvent string
	Agent       string
	Command     string
	Args        []string
}

type tesslDocument struct {
	Name         string                     `json:"name"`
	Mode         string                     `json:"mode"`
	Dependencies map[string]tesslDependency `json:"dependencies"`
}

type tesslDependency struct {
	Version string `json:"version"`
}

type pluginDocument struct {
	Name        string                              `json:"name"`
	Version     string                              `json:"version"`
	Description string                              `json:"description"`
	Repository  json.RawMessage                     `json:"repository"`
	Skills      json.RawMessage                     `json:"skills"`
	Rules       json.RawMessage                     `json:"rules"`
	Hooks       map[string][]pluginGroup            `json:"hooks"`
	NativeHooks map[string]map[string][]pluginGroup `json:"nativeHooks"`
}

type pluginGroup struct {
	Hooks []pluginCommand `json:"hooks"`
}

type pluginCommand struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type tileDocument struct {
	Name    string               `json:"name"`
	Version string               `json:"version"`
	Summary string               `json:"summary"`
	Skills  map[string]tileSkill `json:"skills"`
	Rules   map[string]tileRule  `json:"rules"`
}

type tileSkill struct {
	Path string `json:"path"`
}

type tileRule struct {
	Rules string `json:"rules"`
}

// LoadInstalls reads tessl.json and every installed plugin or tile manifest.
func LoadInstalls(snapshot adapter.Snapshot) ([]PackageInstall, error) {
	directories, err := directorySnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	identities, err := discoverIdentities(directories)
	if err != nil {
		return nil, err
	}
	installs := make([]PackageInstall, 0, len(identities))
	for _, identity := range identities {
		install, ok, err := loadInstall(directories, identity)
		if err != nil {
			return nil, err
		}
		if ok {
			installs = append(installs, install)
		}
	}
	sort.Slice(installs, func(left, right int) bool { return installs[left].Name < installs[right].Name })
	return installs, nil
}

func discoverIdentities(snapshot adapter.DirectorySnapshot) ([]string, error) {
	seen := make(map[string]struct{})
	content, present, err := readOptional(snapshot, tesslJSONPath)
	if err != nil {
		return nil, err
	}
	if present {
		var document tesslDocument
		if err := json.Unmarshal(content, &document); err != nil {
			return nil, fmt.Errorf("decode %s: %w; repair tessl.json or reinstall plugins", tesslJSONPath, err)
		}
		for identity := range document.Dependencies {
			if identity != "" {
				seen[identity] = struct{}{}
			}
		}
	}
	workspaces, err := readDir(snapshot, tesslPluginsRoot)
	if err != nil {
		return nil, err
	}
	for _, workspace := range workspaces {
		if !workspace.Mode.IsDir() || workspace.Mode&fs.ModeSymlink != 0 {
			continue
		}
		packages, err := readDir(snapshot, workspace.Path)
		if err != nil {
			return nil, err
		}
		for _, pkg := range packages {
			if !pkg.Mode.IsDir() || pkg.Mode&fs.ModeSymlink != 0 {
				continue
			}
			identity := path.Base(workspace.Path) + "/" + path.Base(pkg.Path)
			seen[identity] = struct{}{}
		}
	}
	identities := make([]string, 0, len(seen))
	for identity := range seen {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	return identities, nil
}

func loadInstall(snapshot adapter.DirectorySnapshot, identity string) (PackageInstall, bool, error) {
	root := posixJoin(tesslPluginsRoot, identity)
	pluginPath := posixJoin(root, pluginManifestRel)
	tilePath := posixJoin(root, tileManifestName)
	pluginContent, hasPlugin, err := readOptional(snapshot, pluginPath)
	if err != nil {
		return PackageInstall{}, false, err
	}
	tileContent, hasTile, err := readOptional(snapshot, tilePath)
	if err != nil {
		return PackageInstall{}, false, err
	}
	if !hasPlugin && !hasTile {
		return PackageInstall{}, false, nil
	}

	install := PackageInstall{
		TesslIdentity:    identity,
		Name:             identity,
		ManifestKind:     tileManifest,
		PackageMapping:   mappingUnmapped,
		MappingCandidate: "github:" + identity,
		Root:             root,
	}
	var plugin pluginDocument
	if hasPlugin {
		if err := json.Unmarshal(pluginContent, &plugin); err != nil {
			return PackageInstall{}, false, fmt.Errorf("decode %s: %w; repair the plugin manifest or reinstall %s", pluginPath, err, identity)
		}
		install.ManifestKind = pluginManifest
		if plugin.Name != "" {
			install.TesslIdentity = plugin.Name
			install.Name = plugin.Name
			install.MappingCandidate = "github:" + plugin.Name
		}
		install.Version = plugin.Version
		install.Name, install.PackageMapping, install.MappingCandidate = mappingFromRepository(plugin.Repository, install.TesslIdentity)
	}
	var tile tileDocument
	if hasTile {
		if err := json.Unmarshal(tileContent, &tile); err != nil {
			return PackageInstall{}, false, fmt.Errorf("decode %s: %w; repair the tile manifest or reinstall %s", tilePath, err, identity)
		}
		if !hasPlugin {
			if tile.Name != "" {
				install.TesslIdentity = tile.Name
				install.Name = tile.Name
				install.MappingCandidate = "github:" + tile.Name
			}
			install.Version = tile.Version
		}
	}

	pluginRules, pluginSkills, err := pluginArtifacts(snapshot, root, plugin, hasPlugin)
	if err != nil {
		return PackageInstall{}, false, err
	}
	tileRules, tileSkills := tileArtifacts(tile, hasTile)
	install.Rules = mergeDeclared(pluginRules, tileRules)
	install.Skills = mergeDeclared(pluginSkills, tileSkills)
	if hasPlugin {
		install.Hooks = pluginHooks(plugin)
	}
	return install, true, nil
}

func mappingFromRepository(raw json.RawMessage, tesslIdentity string) (name, mapping, candidate string) {
	name = tesslIdentity
	mapping = mappingUnmapped
	candidate = "github:" + tesslIdentity
	if len(raw) == 0 || string(raw) == "null" {
		return name, mapping, candidate
	}
	var url string
	if err := json.Unmarshal(raw, &url); err == nil {
		return mappingFromGitHubURL(url, tesslIdentity)
	}
	var object struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.URL != "" {
		return mappingFromGitHubURL(object.URL, tesslIdentity)
	}
	return name, mapping, candidate
}

func mappingFromGitHubURL(url, tesslIdentity string) (name, mapping, candidate string) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(url, prefix) {
		return tesslIdentity, mappingUnmapped, "github:" + tesslIdentity
	}
	repo := strings.TrimSuffix(strings.TrimPrefix(url, prefix), ".git")
	repo = strings.Trim(repo, "/")
	if repo == "" || strings.Count(repo, "/") != 1 {
		return tesslIdentity, mappingUnmapped, "github:" + tesslIdentity
	}
	return repo, mappingGitHub, "github:" + repo
}

func pluginArtifacts(snapshot adapter.DirectorySnapshot, root string, plugin pluginDocument, present bool) ([]DeclaredPath, []DeclaredPath, error) {
	if !present {
		return nil, nil, nil
	}
	rulePaths, err := decodePathList(plugin.Rules)
	if err != nil {
		return nil, nil, fmt.Errorf("%s rules: %w", posixJoin(root, pluginManifestRel), err)
	}
	skillPaths, err := decodePathList(plugin.Skills)
	if err != nil {
		return nil, nil, fmt.Errorf("%s skills: %w", posixJoin(root, pluginManifestRel), err)
	}
	rules, err := expandPluginRules(snapshot, root, rulePaths)
	if err != nil {
		return nil, nil, err
	}
	skills, err := expandPluginSkills(snapshot, root, skillPaths)
	if err != nil {
		return nil, nil, err
	}
	return rules, skills, nil
}

func decodePathList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, nil
		}
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("must be a directory path or an array of paths")
	}
	return many, nil
}

func expandPluginRules(snapshot adapter.DirectorySnapshot, root string, declared []string) ([]DeclaredPath, error) {
	var result []DeclaredPath
	for _, relative := range declared {
		relative = strings.TrimSuffix(strings.TrimSpace(relative), "/")
		if !validPluginRelPath(relative) {
			return nil, fmt.Errorf("declared rule path %q is not a package-relative POSIX path", relative)
		}
		full := posixJoin(root, relative)
		if strings.HasSuffix(relative, ".md") {
			result = append(result, DeclaredPath{ID: ruleID(relative), Path: relative, FromPlugin: true})
			continue
		}
		entries, err := readDir(snapshot, full)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			result = append(result, DeclaredPath{ID: ruleID(relative), Path: relative, FromPlugin: true})
			continue
		}
		for _, entry := range entries {
			if entry.Mode&fs.ModeSymlink != 0 || !entry.Mode.IsRegular() || !strings.HasSuffix(entry.Path, ".md") {
				continue
			}
			rel := strings.TrimPrefix(entry.Path, root+"/")
			result = append(result, DeclaredPath{ID: ruleID(rel), Path: rel, FromPlugin: true})
		}
	}
	return result, nil
}

func expandPluginSkills(snapshot adapter.DirectorySnapshot, root string, declared []string) ([]DeclaredPath, error) {
	var result []DeclaredPath
	for _, relative := range declared {
		relative = strings.TrimSuffix(strings.TrimSpace(relative), "/")
		if !validPluginRelPath(relative) {
			return nil, fmt.Errorf("declared skill path %q is not a package-relative POSIX path", relative)
		}
		full := posixJoin(root, relative)
		skill, ok, err := declaredSkillPath(snapshot, relative, full)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, skill)
			continue
		}
		children, err := expandSkillContainer(snapshot, root, full)
		if err != nil {
			return nil, err
		}
		if len(children) > 0 {
			result = append(result, children...)
			continue
		}
		result = append(result, DeclaredPath{ID: skillIDFromDir(relative), Path: relative, FromPlugin: true})
	}
	return result, nil
}

func declaredSkillPath(snapshot adapter.Snapshot, relative, directory string) (DeclaredPath, bool, error) {
	present, err := hasSkillMarkdown(snapshot, directory)
	if err != nil {
		return DeclaredPath{}, false, err
	}
	if !present {
		return DeclaredPath{}, false, nil
	}
	return DeclaredPath{ID: skillIDFromDir(relative), Path: relative, FromPlugin: true}, true, nil
}

func expandSkillContainer(snapshot adapter.DirectorySnapshot, root, directory string) ([]DeclaredPath, error) {
	entries, err := readDir(snapshot, directory)
	if err != nil {
		return nil, err
	}
	var children []DeclaredPath
	for _, entry := range entries {
		if entry.Mode&fs.ModeSymlink != 0 || !entry.Mode.IsDir() {
			continue
		}
		child, ok, childErr := declaredSkillPath(snapshot, strings.TrimPrefix(entry.Path, root+"/"), entry.Path)
		if childErr != nil {
			return nil, childErr
		}
		if ok {
			children = append(children, child)
		}
	}
	return children, nil
}

func hasSkillMarkdown(snapshot adapter.Snapshot, directory string) (bool, error) {
	_, present, err := readOptional(snapshot, posixJoin(directory, "SKILL.md"))
	return present, err
}

func tileArtifacts(tile tileDocument, present bool) ([]DeclaredPath, []DeclaredPath) {
	if !present {
		return nil, nil
	}
	rules := make([]DeclaredPath, 0, len(tile.Rules))
	for id, rule := range tile.Rules {
		rules = append(rules, DeclaredPath{ID: id, Path: rule.Rules, FromTile: true})
	}
	skills := make([]DeclaredPath, 0, len(tile.Skills))
	for id, skill := range tile.Skills {
		skills = append(skills, DeclaredPath{ID: id, Path: skillDirectory(skill.Path), FromTile: true})
	}
	return rules, skills
}

func skillDirectory(declared string) string {
	cleaned := path.Clean(declared)
	if path.Base(cleaned) == "SKILL.md" {
		return path.Dir(cleaned)
	}
	return cleaned
}

func mergeDeclared(pluginItems, tileItems []DeclaredPath) []DeclaredPath {
	byID := make(map[string]DeclaredPath)
	order := make([]string, 0)
	add := func(item DeclaredPath) {
		existing, ok := byID[item.ID]
		if !ok {
			byID[item.ID] = item
			order = append(order, item.ID)
			return
		}
		existing.FromPlugin = existing.FromPlugin || item.FromPlugin
		existing.FromTile = existing.FromTile || item.FromTile
		if item.FromPlugin {
			if existing.FromTile && existing.Path != item.Path {
				existing.Ambiguous = true
			}
			existing.Path = item.Path
		} else if existing.FromPlugin && existing.Path != item.Path {
			existing.Ambiguous = true
		}
		byID[item.ID] = existing
	}
	for _, item := range pluginItems {
		add(item)
	}
	for _, item := range tileItems {
		add(item)
	}
	result := make([]DeclaredPath, 0, len(order))
	sort.Strings(order)
	for _, id := range order {
		result = append(result, byID[id])
	}
	return result
}

func pluginHooks(plugin pluginDocument) []DeclaredHook {
	var hooks []DeclaredHook
	events := make([]string, 0, len(plugin.Hooks))
	for event := range plugin.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		for _, group := range plugin.Hooks[event] {
			for _, command := range group.Hooks {
				hooks = append(hooks, DeclaredHook{
					ID:          hookID(command),
					NativeEvent: event,
					Command:     command.Command,
					Args:        append([]string(nil), command.Args...),
				})
			}
		}
	}
	agents := make([]string, 0, len(plugin.NativeHooks))
	for agent := range plugin.NativeHooks {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	for _, agent := range agents {
		events := make([]string, 0, len(plugin.NativeHooks[agent]))
		for event := range plugin.NativeHooks[agent] {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			for _, group := range plugin.NativeHooks[agent][event] {
				for _, command := range group.Hooks {
					hooks = append(hooks, DeclaredHook{
						ID:          hookID(command),
						NativeEvent: event,
						Agent:       agent,
						Command:     command.Command,
						Args:        append([]string(nil), command.Args...),
					})
				}
			}
		}
	}
	return hooks
}

func hookID(command pluginCommand) string {
	relpath, ok := hookRelPath(command.Command, command.Args)
	if !ok {
		if command.Command != "" {
			return sanitizeID(path.Base(command.Command))
		}
		return "hook"
	}
	return sanitizeID(strings.TrimSuffix(path.Base(relpath), path.Ext(relpath)))
}

func hookRelPath(command string, args []string) (string, bool) {
	parsed := parseHookCommand(command, args)
	return parsed.RelPath, parsed.OK
}

func ruleID(relative string) string {
	return sanitizeID(strings.TrimSuffix(path.Base(relative), path.Ext(relative)))
}

func skillIDFromDir(relative string) string {
	return sanitizeID(path.Base(relative))
}

func sanitizeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == "/" {
		return "artifact"
	}
	return value
}
