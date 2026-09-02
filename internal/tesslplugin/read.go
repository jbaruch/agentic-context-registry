package tesslplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

const (
	pluginManifestRel = ".tessl-plugin/plugin.json"
	tileManifestName  = "tile.json"
	pluginManifest    = "plugin.json"
	tileManifest      = "tile.json"
)

// Author is Tessl provenance metadata with no #4 home.
type Author struct {
	Name  string
	Email string
	URL   string
}

// NamedPath is a tile.json map entry: the key is the live artifact identity.
type NamedPath struct {
	ID   string
	Path string
}

// PathSpecKind identifies which Tessl path form a field used.
type PathSpecKind int

const (
	PathSpecEmpty PathSpecKind = iota
	PathSpecDirectory
	PathSpecList
	PathSpecNamed
)

// PathSpec is one Tessl rules or skills field after closed decoding.
type PathSpec struct {
	Kind      PathSpecKind
	Directory string
	List      []string
	Named     []NamedPath
}

// HookCommand is one Tessl command hook.
type HookCommand struct {
	Type    string
	Command string
	Args    []string
}

// HookGroup is one Tessl matcher group.
type HookGroup struct {
	Matcher string
	Hooks   []HookCommand
}

// PluginManifest is a decoded .tessl-plugin/plugin.json.
type PluginManifest struct {
	Name        string
	Version     string
	Description string
	Repository  string
	Homepage    string
	License     string
	Author      *Author
	Private     *bool
	Rules       PathSpec
	Skills      PathSpec
	Hooks       map[string][]HookGroup
	NativeHooks map[string]map[string][]HookGroup
}

// TileManifest is a decoded tile.json.
type TileManifest struct {
	Name       string
	Version    string
	Summary    string
	Repository string
	Homepage   string
	License    string
	Author     *Author
	Private    *bool
	Rules      PathSpec
	Skills     PathSpec
}

// Sources is the Tessl evidence present at a package root.
type Sources struct {
	Root       string
	Plugin     *PluginManifest
	PluginPath string
	Tile       *TileManifest
	TilePath   string
}

// Read loads tile.json and plugin.json with closed JSON decoders.
func Read(packageRoot string) (sources Sources, err error) {
	root, err := os.OpenRoot(packageRoot)
	if err != nil {
		return Sources{}, fmt.Errorf("open package root %s: %w", packageRoot, err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close package root %s: %w", packageRoot, closeErr))
		}
	}()

	sources.Root = packageRoot
	pluginData, pluginErr := readOptionalFile(root, pluginManifestRel)
	if pluginErr != nil {
		return Sources{}, pluginErr
	}
	if pluginData != nil {
		plugin, err := decodePlugin(pluginData)
		if err != nil {
			return Sources{}, err
		}
		sources.Plugin = &plugin
		sources.PluginPath = pluginManifestRel
	}

	tileData, tileErr := readOptionalFile(root, tileManifestName)
	if tileErr != nil {
		return Sources{}, tileErr
	}
	if tileData != nil {
		tile, err := decodeTile(tileData)
		if err != nil {
			return Sources{}, err
		}
		sources.Tile = &tile
		sources.TilePath = tileManifestName
	}

	if sources.Plugin == nil && sources.Tile == nil {
		return Sources{}, fmt.Errorf("no Tessl plugin manifest found in %s; add %s or %s", packageRoot, pluginManifestRel, tileManifestName)
	}
	return sources, nil
}

func readOptionalFile(root *os.Root, relative string) (data []byte, err error) {
	file, err := root.Open(relative)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", relative, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", relative, closeErr))
		}
	}()
	data, err = io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", relative, err)
	}
	return data, nil
}

type jsonAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	URL   string `json:"url"`
}

type jsonHookCommand struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type jsonHookGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []jsonHookCommand `json:"hooks"`
}

type jsonPlugin struct {
	Name        string                                `json:"name"`
	Version     string                                `json:"version"`
	Description string                                `json:"description"`
	Repository  json.RawMessage                       `json:"repository"`
	Homepage    string                                `json:"homepage"`
	License     string                                `json:"license"`
	Author      *jsonAuthor                           `json:"author"`
	Private     *bool                                 `json:"private"`
	Rules       json.RawMessage                       `json:"rules"`
	Skills      json.RawMessage                       `json:"skills"`
	Hooks       map[string][]jsonHookGroup            `json:"hooks"`
	NativeHooks map[string]map[string][]jsonHookGroup `json:"nativeHooks"`
}

type jsonTile struct {
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	Summary    string          `json:"summary"`
	Repository json.RawMessage `json:"repository"`
	Homepage   string          `json:"homepage"`
	License    string          `json:"license"`
	Author     *jsonAuthor     `json:"author"`
	Private    *bool           `json:"private"`
	Rules      json.RawMessage `json:"rules"`
	Skills     json.RawMessage `json:"skills"`
}

type jsonTileRule struct {
	Rules string `json:"rules"`
}

type jsonTileSkill struct {
	Path string `json:"path"`
}

type jsonRepositoryObject struct {
	URL string `json:"url"`
}

func decodePlugin(data []byte) (PluginManifest, error) {
	var raw jsonPlugin
	if err := decodeClosedJSON(data, &raw, pluginManifestRel); err != nil {
		return PluginManifest{}, err
	}
	repository, err := decodeRepository(raw.Repository, pluginManifestRel)
	if err != nil {
		return PluginManifest{}, err
	}
	rules, err := decodePathSpec(raw.Rules, "rules", pluginManifestRel, true)
	if err != nil {
		return PluginManifest{}, err
	}
	skills, err := decodePathSpec(raw.Skills, "skills", pluginManifestRel, false)
	if err != nil {
		return PluginManifest{}, err
	}
	return PluginManifest{
		Name:        raw.Name,
		Version:     raw.Version,
		Description: raw.Description,
		Repository:  repository,
		Homepage:    raw.Homepage,
		License:     raw.License,
		Author:      decodeAuthor(raw.Author),
		Private:     raw.Private,
		Rules:       rules,
		Skills:      skills,
		Hooks:       decodeHookMap(raw.Hooks),
		NativeHooks: decodeNativeHookMap(raw.NativeHooks),
	}, nil
}

func decodeTile(data []byte) (TileManifest, error) {
	var raw jsonTile
	if err := decodeClosedJSON(data, &raw, tileManifestName); err != nil {
		return TileManifest{}, err
	}
	repository, err := decodeRepository(raw.Repository, tileManifestName)
	if err != nil {
		return TileManifest{}, err
	}
	rules, err := decodePathSpec(raw.Rules, "rules", tileManifestName, true)
	if err != nil {
		return TileManifest{}, err
	}
	skills, err := decodePathSpec(raw.Skills, "skills", tileManifestName, false)
	if err != nil {
		return TileManifest{}, err
	}
	return TileManifest{
		Name:       raw.Name,
		Version:    raw.Version,
		Summary:    raw.Summary,
		Repository: repository,
		Homepage:   raw.Homepage,
		License:    raw.License,
		Author:     decodeAuthor(raw.Author),
		Private:    raw.Private,
		Rules:      rules,
		Skills:     skills,
	}, nil
}

func decodeClosedJSON(data []byte, dest any, source string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return classifyJSONError(err, source)
	}
	if decoder.More() {
		return conversionError(CodeUnknownField, source, "trailing JSON in %s is not supported; keep a single manifest object", source)
	}
	return nil
}

func classifyJSONError(err error, source string) error {
	message := err.Error()
	const prefix = "json: unknown field "
	if strings.HasPrefix(message, prefix) {
		field := strings.Trim(strings.TrimPrefix(message, prefix), `"`)
		return conversionError(CodeUnknownField, field, "unknown field %q in %s; remove it or map it before converting", field, source)
	}
	return fmt.Errorf("decode %s: %w", source, err)
}

func decodeAuthor(raw *jsonAuthor) *Author {
	if raw == nil {
		return nil
	}
	return &Author{Name: raw.Name, Email: raw.Email, URL: raw.URL}
}

func decodeRepository(raw json.RawMessage, source string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var url string
	if err := unmarshalClosed(raw, &url, source+".repository"); err == nil {
		return url, nil
	}
	var object jsonRepositoryObject
	if err := unmarshalClosed(raw, &object, source+".repository"); err != nil {
		return "", conversionError(CodeUnknownField, "repository", "repository in %s must be a URL string or {\"url\":\"...\"}", source)
	}
	return object.URL, nil
}

func unmarshalClosed(raw json.RawMessage, dest any, field string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return classifyJSONError(err, field)
	}
	return nil
}

func decodePathSpec(raw json.RawMessage, field, source string, rules bool) (PathSpec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return PathSpec{}, nil
	}
	var directory string
	if err := unmarshalClosed(raw, &directory, source+"."+field); err == nil {
		if directory == "" {
			return PathSpec{}, nil
		}
		if rules && strings.HasSuffix(directory, ".md") {
			return PathSpec{Kind: PathSpecList, List: []string{directory}}, nil
		}
		return PathSpec{Kind: PathSpecDirectory, Directory: directory}, nil
	}
	var list []string
	if err := unmarshalClosed(raw, &list, source+"."+field); err == nil {
		return PathSpec{Kind: PathSpecList, List: append([]string(nil), list...)}, nil
	}
	if rules {
		var named map[string]jsonTileRule
		if err := unmarshalClosed(raw, &named, source+"."+field); err != nil {
			return PathSpec{}, conversionError(CodeUnknownField, field, "%s.%s must be a directory path, an array of paths, or a named map", source, field)
		}
		return namedRuleSpec(named), nil
	}
	var named map[string]jsonTileSkill
	if err := unmarshalClosed(raw, &named, source+"."+field); err != nil {
		return PathSpec{}, conversionError(CodeUnknownField, field, "%s.%s must be a directory path, an array of paths, or a named map", source, field)
	}
	return namedSkillSpec(named), nil
}

func namedRuleSpec(named map[string]jsonTileRule) PathSpec {
	keys := make([]string, 0, len(named))
	for key := range named {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := PathSpec{Kind: PathSpecNamed, Named: make([]NamedPath, 0, len(keys))}
	for _, key := range keys {
		result.Named = append(result.Named, NamedPath{ID: key, Path: named[key].Rules})
	}
	return result
}

func namedSkillSpec(named map[string]jsonTileSkill) PathSpec {
	keys := make([]string, 0, len(named))
	for key := range named {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := PathSpec{Kind: PathSpecNamed, Named: make([]NamedPath, 0, len(keys))}
	for _, key := range keys {
		result.Named = append(result.Named, NamedPath{ID: key, Path: named[key].Path})
	}
	return result
}

func decodeHookMap(raw map[string][]jsonHookGroup) map[string][]HookGroup {
	if raw == nil {
		return nil
	}
	result := make(map[string][]HookGroup, len(raw))
	for event, groups := range raw {
		result[event] = decodeHookGroups(groups)
	}
	return result
}

func decodeNativeHookMap(raw map[string]map[string][]jsonHookGroup) map[string]map[string][]HookGroup {
	if raw == nil {
		return nil
	}
	result := make(map[string]map[string][]HookGroup, len(raw))
	for agent, events := range raw {
		copied := make(map[string][]HookGroup, len(events))
		for event, groups := range events {
			copied[event] = decodeHookGroups(groups)
		}
		result[agent] = copied
	}
	return result
}

func decodeHookGroups(groups []jsonHookGroup) []HookGroup {
	result := make([]HookGroup, 0, len(groups))
	for _, group := range groups {
		converted := HookGroup{Matcher: group.Matcher, Hooks: make([]HookCommand, 0, len(group.Hooks))}
		for _, command := range group.Hooks {
			converted.Hooks = append(converted.Hooks, HookCommand{
				Type:    command.Type,
				Command: command.Command,
				Args:    append([]string(nil), command.Args...),
			})
		}
		result = append(result, converted)
	}
	return result
}
