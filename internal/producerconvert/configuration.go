package producerconvert

import (
	"io/fs"
	"path"
	"regexp"
)

var checkerPythonVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// These nonexecutable configuration formats describe repository inputs rather
// than runtime invocations. Keep their complete bytes and ordering; do not offer
// them to the provider or rebase checker directories into native file paths.
func preservedConfiguration(name string, state fileState, source tree) bool {
	if state.Directory || state.Link != "" || state.Mode&0111 != 0 {
		return false
	}
	if path.Base(name) == ".gitignore" {
		return true
	}
	if path.Base(name) != "pyrightconfig.json" {
		return false
	}
	var config struct {
		Include              []string `json:"include"`
		PythonVersion        string   `json:"pythonVersion"`
		TypeCheckingMode     string   `json:"typeCheckingMode"`
		ReportMissingImports string   `json:"reportMissingImports"`
	}
	if strictJSON(state.Content, &config) != nil || len(config.Include) == 0 || !checkerPythonVersion.MatchString(config.PythonVersion) {
		return false
	}
	if config.TypeCheckingMode != "standard" && config.TypeCheckingMode != "strict" && config.TypeCheckingMode != "basic" {
		return false
	}
	if config.ReportMissingImports != "error" && config.ReportMissingImports != "warning" && config.ReportMissingImports != "none" {
		return false
	}
	for _, root := range config.Include {
		if !fs.ValidPath(root) || !source[path.Join(path.Dir(name), root)].Directory {
			return false
		}
	}
	return true
}
