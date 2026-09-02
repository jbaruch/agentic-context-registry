// Package freshness defines the deterministic session-start freshness policy
// and its generated hook package.
package freshness

// Policy controls what the generated session-start hook does.
type Policy string

const (
	// PolicyOutdated checks latest dependency identities without writing the project.
	PolicyOutdated Policy = "outdated"
	// PolicyInstall reconciles latest dependencies and realizes changed artifacts.
	PolicyInstall Policy = "install"
	// PolicyNone removes the generated session-start hook.
	PolicyNone Policy = "none"
)

// Resolve chooses the effective policy and reports whether agents.yaml should
// persist it. An explicit flag wins, followed by stored configuration; an
// unconfigured project deterministically defaults to outdated.
func Resolve(stored, flag string, explicit bool) (Policy, bool) {
	if explicit {
		return Policy(flag), true
	}
	if stored != "" {
		return Policy(stored), false
	}
	return PolicyOutdated, true
}
