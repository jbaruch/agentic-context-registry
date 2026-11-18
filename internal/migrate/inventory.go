package migrate

import (
	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// SourcePackageRepairRemedy directs repairs to authored content rather than
// the consumer's installed copy, shared by inventory and vendor planning.
const SourcePackageRepairRemedy = "fix the reported problem in the source package, ask its publisher if needed, reinstall or update through Tessl, then rerun migration"

// Inventory reads a Tessl consumer project and returns a dry-run report. It
// never writes files; the only filesystem handle a caller should supply is a
// read-only RootSnapshot.
func Inventory(snapshot adapter.Snapshot) (Report, error) {
	report := emptyReport()
	if err := RefuseSymlinkedSharedSurface(snapshot); err != nil {
		return Report{}, err
	}
	installs, err := LoadInstalls(snapshot)
	if err != nil {
		return Report{}, err
	}
	var extraFiles []string
	for _, install := range installs {
		pkg, extras, err := inventoryPackage(snapshot, install)
		if err != nil {
			return Report{}, err
		}
		report.Packages = append(report.Packages, pkg)
		extraFiles = append(extraFiles, extras...)
	}
	markDuplicateSkills(&report)
	if err := classifyProject(snapshot, installs, extraFiles, &report); err != nil {
		return Report{}, err
	}
	sortReport(&report)
	return report, nil
}

func inventoryPackage(snapshot adapter.Snapshot, install PackageInstall) (PackageReport, []string, error) {
	pkg := PackageReport{
		Name:             install.Name,
		TesslIdentity:    install.TesslIdentity,
		Version:          install.Version,
		Manifest:         install.ManifestKind,
		PackageMapping:   install.PackageMapping,
		MappingCandidate: install.MappingCandidate,
		Artifacts:        []ArtifactReport{},
	}
	rules, err := NormalizeRules(snapshot, install)
	if err != nil {
		return PackageReport{}, nil, err
	}
	skills, err := NormalizeSkills(snapshot, install)
	if err != nil {
		return PackageReport{}, nil, err
	}
	hooks, err := NormalizeHooks(snapshot, install)
	if err != nil {
		return PackageReport{}, nil, err
	}
	for _, rule := range rules {
		artifact := ArtifactReport{
			ID:             rule.ID,
			Kind:           kindRule,
			Classification: classMigratable,
			Digest:         rule.Digest,
			Lossy:          rule.Lossy,
			Natives:        rule.Natives,
		}
		if rule.Activation.Mode != "" {
			artifact.Activation = &ActivationReport{Mode: string(rule.Activation.Mode), Paths: rule.Activation.Paths}
		}
		if rule.Ambiguous {
			artifact.Classification = classAmbiguous
		}
		pkg.Artifacts = append(pkg.Artifacts, artifact)
	}
	var extras []string
	for _, skill := range skills {
		if skill.Reason == ReasonRootSkill {
			pkg.Reason = ReasonRootSkill
			pkg.Remedy = "in the source package, move SKILL.md and its required scripts, references, and assets into " + DiagnosticToken("skills/"+skill.ID) + " and update the skill declaration; keep package metadata and unrelated artifacts outside that directory; ask the publisher if needed, reinstall or update through Tessl, then rerun migration; alternatively, uninstall this package through Tessl, migrate the remaining packages, and add the skill to the project by hand"
		}
		artifact := ArtifactReport{
			ID:             skill.ID,
			Kind:           kindSkill,
			Classification: classMigratable,
			Digest:         skill.Digest,
			Natives:        skill.Natives,
		}
		switch {
		case skill.Unsupported:
			artifact.Classification = classUnsupported
		case skill.Ambiguous:
			artifact.Classification = classAmbiguous
		}
		pkg.Artifacts = append(pkg.Artifacts, artifact)
		extras = append(extras, skill.ExtraFiles...)
	}
	for _, hook := range hooks {
		artifact := ArtifactReport{
			ID:             hook.ID,
			Kind:           kindHook,
			Classification: classMigratable,
			Event:          string(hook.Event),
			Digest:         hook.Digest,
			Natives:        hook.Natives,
		}
		switch {
		case hook.Unsupported:
			artifact.Classification = classUnsupported
		case hook.Ambiguous:
			artifact.Classification = classAmbiguous
		}
		if hook.Event == manifest.HookEvent("") {
			artifact.Event = ""
		}
		pkg.Artifacts = append(pkg.Artifacts, artifact)
	}
	return pkg, extras, nil
}
