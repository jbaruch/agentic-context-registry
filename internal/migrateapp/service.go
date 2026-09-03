package migrateapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

// Error is a named migration failure suitable for the CLI JSON envelope.
type Error struct {
	Code    string
	Message string
	Cause   error
	Remedy  string
}

func (err *Error) Error() string { return err.Message }
func (err *Error) Unwrap() error { return err.Cause }

// Options controls one coexistence migration.
type Options struct {
	DryRun         bool
	Finalize       bool
	VendorUnmapped bool
	FileMappings   []migrate.Mapping
	CLIMappings    []migrate.Mapping
}

// Service inventories and migrates Tessl consumers and converts Tessl plugin packages.
type Service struct {
	github        dependency.GitHub
	resolver      *dependency.Resolver
	realizer      *realizeapp.Service
	applyVendor   func(string, migrate.VendorPlan) (bool, func() error, error)
	removeVendors func(string, []vendorSupersede) error
}

// NewService constructs the read-only inventory service retained for callers
// that do not need network-backed migration.
func NewService() *Service { return &Service{} }

func newService(github dependency.GitHub) *Service {
	resolver := dependency.NewResolver(github)
	return &Service{
		github: github, resolver: resolver, realizer: realizeapp.NewService(resolver),
		applyVendor: applyVendorPlan, removeVendors: removeSupersededVendors,
	}
}

// Inventory opens a root-confined snapshot and returns the dry-run report.
func (service *Service) Inventory(projectDirectory string) (report migrate.Report, err error) {
	snapshot, err := adapter.NewRootSnapshot(projectDirectory)
	if err != nil {
		return migrate.Report{}, fmt.Errorf("open project %q: %w", projectDirectory, err)
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close project %q: %w", projectDirectory, closeErr))
		}
	}()
	return migrate.Inventory(snapshot)
}

// Convert runs producer conversion for one package root.
func (service *Service) Convert(opts tesslplugin.Options) (tesslplugin.Report, error) {
	if opts.PackageRoot == "" {
		opts.PackageRoot = "."
	}
	return tesslplugin.Convert(opts)
}

// Migrate resolves mapped packages, plans their ACR-native realization, and
// applies only ACR-owned state in coexistence mode.
func (service *Service) Migrate(ctx context.Context, projectDirectory string, options Options) (migrate.MigrationReport, error) {
	if service.github == nil || service.resolver == nil || service.realizer == nil {
		return migrate.MigrationReport{}, errors.New("migration service requires a GitHub resolver")
	}
	if !options.DryRun {
		if err := realize.RecoverTransactions(projectDirectory); err != nil {
			return migrate.MigrationReport{}, err
		}
	}
	manifestPath := filepath.Join(projectDirectory, "tessl.json")
	tesslRoot := filepath.Join(projectDirectory, ".tessl")
	_, manifestErr := os.Lstat(manifestPath)
	_, rootErr := os.Lstat(tesslRoot)
	if errors.Is(manifestErr, os.ErrNotExist) && errors.Is(rootErr, os.ErrNotExist) {
		return emptyMigrationReport(options), nil
	}
	if options.Finalize && errors.Is(manifestErr, os.ErrNotExist) && rootErr == nil {
		return migrate.MigrationReport{}, namedError("finalization_blocked", "tessl.json is missing while .tessl still exists; restore the manifest before finalization", manifestErr)
	}
	if manifestErr == nil && errors.Is(rootErr, os.ErrNotExist) {
		report := emptyMigrationReport(options)
		report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "tessl_not_installed", Path: ".tessl"})
		return report, nil
	}
	if info, err := os.Lstat(manifestPath); errors.Is(err, os.ErrNotExist) {
		return migrate.MigrationReport{}, namedError("tessl_manifest_absent", "tessl.json is required to establish live Tessl package ownership", err)
	} else if err != nil {
		return migrate.MigrationReport{}, fmt.Errorf("inspect tessl.json: %w", err)
	} else if !info.Mode().IsRegular() {
		return migrate.MigrationReport{}, namedError("tessl_manifest_absent", "tessl.json must be a regular file", nil)
	}
	inventory, err := service.Inventory(projectDirectory)
	if err != nil {
		return migrate.MigrationReport{}, err
	}
	existing, err := dependency.LoadState(projectDirectory)
	if err != nil {
		return migrate.MigrationReport{}, err
	}
	existingVendors := make(map[string]bool)
	for _, declaration := range existing.Project.Dependencies {
		if identity, parseErr := dependency.ParseVendorSource(declaration.Source); parseErr == nil {
			existingVendors[identity.FullName()] = true
		}
	}
	mappings, err := migrate.ResolveMappingsWithVendorSources(inventory.Packages, options.FileMappings, options.CLIMappings, options.VendorUnmapped, existingVendors)
	if err != nil {
		var unmapped *migrate.UnmappedPackageError
		var conflict *migrate.MappingConflictError
		switch {
		case errors.As(err, &unmapped):
			if options.Finalize {
				return migrate.MigrationReport{}, namedError("finalization_blocked", err.Error(), err)
			}
			return migrate.MigrationReport{}, namedError("unmapped_package", err.Error(), err)
		case errors.As(err, &conflict):
			return migrate.MigrationReport{}, namedError("mapping_conflict", err.Error(), err)
		default:
			return migrate.MigrationReport{}, err
		}
	}
	vendorPlans, err := service.planVendors(projectDirectory, mappings)
	if err != nil {
		return migrate.MigrationReport{}, classifyVendorError(err)
	}
	for _, plan := range vendorPlans {
		if err := service.resolver.RegisterVendorPreview(plan.Source, filepath.Join(projectDirectory, ".tessl", "plugins", filepath.FromSlash(plan.Identity)), plan.Manifest); err != nil {
			return migrate.MigrationReport{}, classifyVendorError(err)
		}
	}
	defer service.resolver.ClearVendorPreviews()
	desired, mappings, err := service.resolveState(ctx, existing, mappings, vendorPlans)
	if err != nil {
		return migrate.MigrationReport{}, err
	}
	desired.Project.Agents = selectedAgents(inventory)
	superseded, err := service.validateSupersedes(ctx, projectDirectory, existing, desired, mappings)
	if err != nil {
		return migrate.MigrationReport{}, err
	}
	if err := compatibleMigrationState(existing, desired, len(superseded) != 0); err != nil {
		return migrate.MigrationReport{}, err
	}
	preview, err := service.realizer.RunState(ctx, projectDirectory, desired, desired.Project.Agents, realize.ModeDryRun)
	if err != nil {
		return migrate.MigrationReport{}, err
	}
	if err := realize.ValidateTesslOwnedTargets(preview.Plan); err != nil {
		return migrate.MigrationReport{}, err
	}
	report, err := service.buildReport(ctx, projectDirectory, inventory, mappings, desired, preview, options.DryRun || options.Finalize)
	if err != nil {
		return migrate.MigrationReport{}, err
	}
	for _, plan := range vendorPlans {
		report.Vendored = append(report.Vendored, migrate.VendoredPackage{Source: plan.Source, Destination: plan.Destination, Version: plan.Version, ContentHash: plan.ContentHash})
	}
	if options.Finalize {
		if !report.FinalizationReady {
			report.Mode = "finalize"
			report.Retained = finalizationRetentions(inventory)
			migrate.SortMigrationReport(&report)
			return report, namedError("finalization_blocked", "Tessl finalization is blocked by the reported diffs, ambiguity, lossiness, mappings, or uncovered agents", nil)
		}
		if preview.Plan.HasChanges() {
			return report, namedError("finalization_blocked", "ACR coexistence state is not current; run 'acr migrate tessl' first, review and commit its output, then finalize", nil)
		}
		versionControlled, err := ensureFinalizationTracked(projectDirectory, desired)
		if err != nil {
			return report, err
		}
		if !versionControlled {
			report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "no-version-control", Detail: "Git tracking checks are not applicable"})
		}
		ledger, err := realize.DecodeLedger(desired.Lock.Realization)
		if err != nil {
			return report, err
		}
		finalizePlan, err := planFinalization(projectDirectory, inventory, ledger)
		if err != nil {
			return report, err
		}
		report.Mode = "finalize"
		report.Removed, report.Retained = finalizationRecords(finalizePlan, ledger)
		report.Reanchored = plannedReanchors(ledger, finalizePlan)
		report.StaleReferences, err = findStaleReferences(projectDirectory, report.Removed)
		if err != nil {
			return report, err
		}
		if options.DryRun {
			report.DryRun = true
			report.Wrote = false
			migrate.SortMigrationReport(&report)
			return report, nil
		}
		reanchored, err := applyFinalization(projectDirectory, &desired, finalizePlan)
		if err != nil {
			var migrationErr *Error
			if errors.As(err, &migrationErr) {
				return report, err
			}
			return report, namedError("finalization_failed", err.Error(), err)
		}
		report.Lock = desired.Lock
		report.Reanchored = reanchored
		report.Mode = "finalized"
		report.DryRun = false
		report.Wrote = len(finalizePlan.Edits) != 0 || len(reanchored) != 0
		migrate.SortMigrationReport(&report)
		return report, nil
	}
	if !options.DryRun {
		vendorChanged, vendorRollbacks, err := service.applyVendorPlans(projectDirectory, vendorPlans)
		if err != nil {
			return migrate.MigrationReport{}, err
		}
		applied, err := service.realizer.RunStateFrom(ctx, projectDirectory, existing, desired, desired.Project.Agents, realize.ModeApply)
		if err != nil {
			return migrate.MigrationReport{}, errors.Join(err, rollbackVendors(vendorRollbacks))
		}
		report.DryRun = false
		report.Wrote = vendorChanged || applied.Plan.HasChanges()
		if len(superseded) != 0 {
			if err := service.removeVendors(projectDirectory, superseded); err != nil {
				live, restoreErr := dependency.LoadState(projectDirectory)
				if restoreErr == nil {
					restore := existing
					restore.Lock.Realization = live.Lock.Realization
					_, restoreErr = service.realizer.RunStateFrom(ctx, projectDirectory, live, restore, desired.Project.Agents, realize.ModeApply)
				}
				if restoreErr != nil {
					restoreErr = fmt.Errorf("restore dependency state and realized outputs after superseded vendor removal failed: %w", restoreErr)
				}
				vendorRollbackErr := rollbackVendors(vendorRollbacks)
				if vendorRollbackErr != nil {
					vendorRollbackErr = fmt.Errorf("roll back newly staged vendor trees: %w", vendorRollbackErr)
				}
				return migrate.MigrationReport{}, errors.Join(err, restoreErr, vendorRollbackErr)
			}
			report.Wrote = true
		}
	}
	migrate.SortMigrationReport(&report)
	return report, nil
}

func finalizationRetentions(inventory migrate.Report) []migrate.RetentionRecord {
	var retained []migrate.RetentionRecord
	for _, record := range inventory.Ambiguous {
		retained = append(retained, migrate.RetentionRecord{Path: record.Path, Reason: record.Reason})
	}
	for _, record := range inventory.Unsupported {
		retained = append(retained, migrate.RetentionRecord{Path: record.Path, Reason: record.Reason})
	}
	for _, pkg := range inventory.Packages {
		for _, artifact := range pkg.Artifacts {
			if artifact.Classification == "migratable" && len(artifact.Lossy) == 0 {
				continue
			}
			for _, native := range artifact.Natives {
				retained = append(retained, migrate.RetentionRecord{Path: native, Kind: artifact.Kind, ID: artifact.ID, Reason: artifact.Classification})
			}
		}
	}
	return retained
}

type vendorSupersede struct {
	removal realize.VendorTreeRemovalPlan
}

var marshalEffectiveDiffs = json.Marshal

func (service *Service) validateSupersedes(ctx context.Context, projectDirectory string, existing, desired dependency.State, mappings []migrate.Mapping) ([]vendorSupersede, error) {
	var result []vendorSupersede
	for _, mapping := range mappings {
		oldSource := "vendor:" + mapping.From
		if mapping.Source == oldSource {
			continue
		}
		oldLock, hasOld := lockBySource(existing.Lock.Dependencies, oldSource)
		if !hasOld {
			continue
		}
		identity, err := dependency.ParseVendorSource(oldSource)
		if err != nil {
			return nil, fmt.Errorf("parse superseded vendor source %q: %w", oldSource, err)
		}
		newLock, hasNew := lockBySource(desired.Lock.Dependencies, mapping.Source)
		if !hasNew {
			continue
		}
		oldPackage, oldCleanup, err := service.resolver.MaterializeLockedAt(ctx, projectDirectory, oldLock)
		if err != nil {
			return nil, classifyResolutionError(oldSource, err)
		}
		newPackage, newCleanup, err := service.resolver.MaterializeLockedAt(ctx, projectDirectory, newLock)
		if err != nil {
			return nil, errors.Join(classifyResolutionError(mapping.Source, err), oldCleanup())
		}
		oldSet, oldErr := migrate.FromPackage(mapping.From, adapter.Package{Source: oldSource, Root: os.DirFS(oldPackage.Root), Manifest: oldPackage.Manifest})
		newSet, newErr := migrate.FromPackage(mapping.From, adapter.Package{Source: mapping.Source, Root: os.DirFS(newPackage.Root), Manifest: newPackage.Manifest})
		cleanupErr := errors.Join(newCleanup(), oldCleanup())
		if oldErr != nil || newErr != nil || cleanupErr != nil {
			return nil, errors.Join(oldErr, newErr, cleanupErr)
		}
		diffs := migrate.CompareEffective(oldSet, newSet)
		if len(diffs) != 0 {
			encoded, err := marshalEffectiveDiffs(diffs)
			if err != nil {
				return nil, fmt.Errorf("encode effective artifact differences: %w", err)
			}
			return nil, namedError("effective_mismatch", fmt.Sprintf("%s cannot supersede %s because effective artifacts differ: %s", mapping.Source, oldSource, encoded), nil)
		}
		destination := filepath.Join(".agents", "vendor", identity.Workspace, identity.Package)
		hash, err := hashVendorTree(filepath.Join(projectDirectory, destination))
		if err != nil {
			return nil, fmt.Errorf("verify %s before supersede removal: %w", oldSource, err)
		}
		if hash != oldLock.ContentHash {
			return nil, namedError("vendor_collision", fmt.Sprintf("refuse to remove modified %s: expected %s, found %s", oldSource, oldLock.ContentHash, hash), nil)
		}
		removal, err := realize.PlanVendorTreeRemoval(projectDirectory, filepath.ToSlash(destination))
		if err != nil {
			return nil, fmt.Errorf("plan superseded %s removal: %w", oldSource, err)
		}
		result = append(result, vendorSupersede{removal: removal})
	}
	return result, nil
}

func compatibleMigrationState(existing, desired dependency.State, superseding bool) error {
	if !superseding {
		return compatibleProjectState(existing, desired)
	}
	copy := existing
	copy.Project.Dependencies = desired.Project.Dependencies
	copy.Lock.Dependencies = desired.Lock.Dependencies
	return compatibleProjectState(copy, desired)
}

func removeSupersededVendors(projectDirectory string, removals []vendorSupersede) error {
	plans := make([]realize.VendorTreeRemovalPlan, 0, len(removals))
	for _, removal := range removals {
		plans = append(plans, removal.removal)
	}
	if err := realize.ApplyVendorTreeRemovals(projectDirectory, plans); err != nil {
		return fmt.Errorf("remove superseded vendor trees: %w", err)
	}
	return nil
}

func (service *Service) planVendors(projectDirectory string, mappings []migrate.Mapping) (plans []migrate.VendorPlan, err error) {
	snapshot, err := adapter.NewRootSnapshot(projectDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	installs, err := migrate.LoadInstalls(snapshot)
	if err != nil {
		return nil, err
	}
	byIdentity := make(map[string]migrate.PackageInstall, len(installs))
	for _, install := range installs {
		byIdentity[install.TesslIdentity] = install
	}
	for _, mapping := range mappings {
		scheme, schemeErr := dependency.SourceScheme(mapping.Source)
		if schemeErr != nil {
			return nil, schemeErr
		}
		if scheme != dependency.SchemeVendor {
			continue
		}
		install, ok := byIdentity[mapping.From]
		if !ok {
			return nil, &migrate.VendorEscapeError{Reason: fmt.Sprintf("installed package %s is missing", mapping.From)}
		}
		plan, planErr := migrate.PlanVendor(snapshot, install)
		if planErr != nil {
			return nil, planErr
		}
		plans = append(plans, plan)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Source < plans[j].Source })
	return plans, nil
}

func rollbackVendors(rollbacks []func() error) error {
	var result error
	for index := len(rollbacks) - 1; index >= 0; index-- {
		result = errors.Join(result, rollbacks[index]())
	}
	return result
}

func classifyVendorError(err error) error {
	if err == nil {
		return nil
	}
	var missingPath *tesslplugin.DeclaredPathError
	if errors.As(err, &missingPath) {
		return &Error{
			Code:    string(manifest.CodePathNotFound),
			Message: err.Error(),
			Cause:   err,
			Remedy:  fmt.Sprintf("create the installed Tessl package's declared %s path %q, remove that declaration from .tessl-plugin/plugin.json, or map the package to a published ACR source", missingPath.Kind, missingPath.Path),
		}
	}
	var escape *migrate.VendorEscapeError
	if errors.As(err, &escape) {
		return namedError("vendor_escape", err.Error(), err)
	}
	var collision *vendorCollisionError
	if errors.As(err, &collision) {
		return namedError("vendor_collision", err.Error(), err)
	}
	var validation *manifest.ValidationErrors
	if errors.As(err, &validation) && len(validation.Issues) != 0 {
		issue := validation.Issues[0]
		return &Error{
			Code:    string(issue.Code),
			Message: err.Error(),
			Cause:   err,
			Remedy:  fmt.Sprintf("repair the installed Tessl package's %s declaration, or map the package to a published ACR source", issue.Field),
		}
	}
	return err
}

func (service *Service) resolveState(ctx context.Context, existing dependency.State, mappings []migrate.Mapping, vendorPlans []migrate.VendorPlan) (dependency.State, []migrate.Mapping, error) {
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.BaselineSchemaVersion, Freshness: existing.Project.Freshness, Extra: existing.Project.Extra},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.BaselineSchemaVersion, Realization: existing.Lock.Realization, Extra: existing.Lock.Extra},
	}
	if len(vendorPlans) != 0 {
		state.Project.SchemaVersion = dependency.VendorSchemaVersion
		state.Lock.SchemaVersion = dependency.VendorSchemaVersion
	}
	planBySource := make(map[string]migrate.VendorPlan, len(vendorPlans))
	for _, plan := range vendorPlans {
		planBySource[plan.Source] = plan
	}
	if state.Project.Freshness == "" {
		state.Project.Freshness = string(freshness.PolicyOutdated)
	}
	seenSources := make(map[string]string)
	for index := range mappings {
		mapping := &mappings[index]
		if previous, exists := seenSources[mapping.Source]; exists && previous != mapping.From {
			return dependency.State{}, nil, namedError("mapping_conflict", fmt.Sprintf("Tessl packages %s and %s both map to %s", previous, mapping.From, mapping.Source), nil)
		}
		seenSources[mapping.Source] = mapping.From
		scheme, err := dependency.SourceScheme(mapping.Source)
		if err != nil {
			return dependency.State{}, nil, err
		}
		if scheme == dependency.SchemeVendor {
			plan, ok := planBySource[mapping.Source]
			if !ok {
				return dependency.State{}, nil, namedError("vendor_escape", fmt.Sprintf("no source tree was found for %s", mapping.Source), nil)
			}
			mapping.Requested = "vendored"
			state.Project.Dependencies = append(state.Project.Dependencies, dependency.Declaration{Source: mapping.Source, Requested: "vendored"})
			state.Lock.Dependencies = append(state.Lock.Dependencies, dependency.LockedDependency{Source: mapping.Source, Requested: "vendored", Kind: dependency.ResolutionVendor, PackageVersion: plan.Version, ContentHash: plan.ContentHash})
			continue
		}
		requested, locked, candidate, reused, err := service.resolveMapping(ctx, existing, *mapping)
		if err != nil {
			return dependency.State{}, nil, err
		}
		mapping.Requested = requested
		declaration := dependency.Declaration{Source: mapping.Source, Requested: requested}
		if previous, ok := declarationBySource(existing.Project.Dependencies, mapping.Source); ok {
			declaration.Hold = previous.Hold
			declaration.Extra = previous.Extra
		}
		state.Project.Dependencies = append(state.Project.Dependencies, declaration)
		if reused {
			state.Lock.Dependencies = append(state.Lock.Dependencies, locked)
			continue
		}
		var resolved dependency.LockedDependency
		if candidate != nil {
			resolved, err = service.resolver.ResolveAt(ctx, declaration, *candidate)
		} else {
			resolved, err = service.resolver.Resolve(ctx, declaration)
		}
		if err != nil {
			return dependency.State{}, nil, classifyResolutionError(mapping.Source, err)
		}
		state.Lock.Dependencies = append(state.Lock.Dependencies, resolved)
	}
	sort.Slice(state.Project.Dependencies, func(i, j int) bool {
		return state.Project.Dependencies[i].Source < state.Project.Dependencies[j].Source
	})
	sort.Slice(state.Lock.Dependencies, func(i, j int) bool { return state.Lock.Dependencies[i].Source < state.Lock.Dependencies[j].Source })
	if err := validateSourceCollisions(state.Project.Dependencies); err != nil {
		return dependency.State{}, nil, err
	}
	return state, mappings, nil
}

func validateSourceCollisions(declarations []dependency.Declaration) error {
	seen := make(map[string]string)
	for _, declaration := range declarations {
		_, identity, found := strings.Cut(declaration.Source, ":")
		if !found {
			continue
		}
		if previous, exists := seen[identity]; exists && previous != declaration.Source {
			return namedError("vendor_collision", fmt.Sprintf("%s and %s derive the same native package name", previous, declaration.Source), nil)
		}
		seen[identity] = declaration.Source
	}
	return nil
}

func (service *Service) resolveMapping(ctx context.Context, existing dependency.State, mapping migrate.Mapping) (string, dependency.LockedDependency, *dependency.Release, bool, error) {
	declaration, hasDeclaration := declarationBySource(existing.Project.Dependencies, mapping.Source)
	locked, hasLock := lockBySource(existing.Lock.Dependencies, mapping.Source)
	if mapping.Explicit {
		if hasDeclaration && hasLock && declaration.Requested == mapping.Requested && locked.Requested == mapping.Requested {
			return mapping.Requested, locked, nil, true, nil
		}
		return mapping.Requested, dependency.LockedDependency{}, nil, false, nil
	}
	if mapping.TesslVersion == "latest" || mapping.TesslVersion == "" {
		if hasDeclaration && hasLock && declaration.Requested == "latest" && locked.Requested == "latest" {
			return "latest", locked, nil, true, nil
		}
		return "latest", dependency.LockedDependency{}, nil, false, nil
	}
	if hasDeclaration && hasLock && declaration.Requested == locked.Requested && dependency.TagMatchesVersion(locked.Tag, mapping.TesslVersion) {
		return declaration.Requested, locked, nil, true, nil
	}
	repository, err := dependency.ParseSource(mapping.Source)
	if err != nil {
		return "", dependency.LockedDependency{}, nil, false, err
	}
	plain, plainErr := service.github.ReleaseByTag(ctx, repository, mapping.TesslVersion)
	prefixed, prefixedErr := service.github.ReleaseByTag(ctx, repository, "v"+mapping.TesslVersion)
	plainFound := plainErr == nil
	prefixedFound := prefixedErr == nil
	if plainErr != nil && !isNotFound(plainErr) {
		return "", dependency.LockedDependency{}, nil, false, plainErr
	}
	if prefixedErr != nil && !isNotFound(prefixedErr) {
		return "", dependency.LockedDependency{}, nil, false, prefixedErr
	}
	if plainFound == prefixedFound {
		code := "tessl_version_unavailable"
		message := fmt.Sprintf("neither %s nor v%s is a release tag for %s", mapping.TesslVersion, mapping.TesslVersion, mapping.Source)
		if plainFound {
			code = "ambiguous_tessl_version"
			message = fmt.Sprintf("both %s and v%s are release tags for %s", mapping.TesslVersion, mapping.TesslVersion, mapping.Source)
		}
		return "", dependency.LockedDependency{}, nil, false, namedError(code, message, nil)
	}
	if prefixedFound {
		return prefixed.Tag, dependency.LockedDependency{}, &prefixed, false, nil
	}
	return plain.Tag, dependency.LockedDependency{}, &plain, false, nil
}

func compatibleProjectState(existing, desired dependency.State) error {
	if len(existing.Project.Agents) != 0 && !reflect.DeepEqual(sortedStrings(existing.Project.Agents), sortedStrings(desired.Project.Agents)) {
		return namedError("project_state_conflict", "existing agents.yaml selects different agents; reconcile it before migration", nil)
	}
	if len(existing.Project.Dependencies) != 0 && !sameDeclarations(existing.Project.Dependencies, desired.Project.Dependencies) {
		return namedError("project_state_conflict", "existing agents.yaml dependencies disagree with the Tessl mapping; reconcile them before migration", nil)
	}
	if len(existing.Lock.Dependencies) != 0 && !sameLocks(existing.Lock.Dependencies, desired.Lock.Dependencies) {
		return namedError("project_state_conflict", "existing registry.lock disagrees with the Tessl mapping; reconcile it before migration", nil)
	}
	return nil
}

func (service *Service) buildReport(ctx context.Context, projectDirectory string, inventory migrate.Report, mappings []migrate.Mapping, state dependency.State, result realizeapp.Result, dryRun bool) (migrate.MigrationReport, error) {
	encodedLedger, err := realize.EncodeLedger(result.Plan.NextLedger)
	if err != nil {
		return migrate.MigrationReport{}, err
	}
	state.Lock.Realization = encodedLedger
	report := migrate.MigrationReport{
		SchemaVersion: 1, DryRun: dryRun, Wrote: !dryRun && result.Plan.HasChanges(), Mode: "coexistence",
		Mappings: append([]migrate.Mapping(nil), mappings...), Project: state.Project, Lock: state.Lock,
		Plan: migrate.MigrationPlan{LedgerChanged: result.Plan.LedgerChanged},
	}
	for _, operation := range result.Plan.Operations {
		report.Plan.Operations = append(report.Plan.Operations, migrate.MigrationOperation{Kind: string(operation.Kind), Path: filepath.ToSlash(operation.Path)})
		if operation.Kind != realize.OperationPreserve {
			report.ToolOwned = appendOwnership(report.ToolOwned, migrate.OwnershipRecord{Path: filepath.ToSlash(operation.Path), Kind: "path"})
		}
	}
	for _, target := range result.Plan.NextLedger.Targets {
		for _, entry := range target.Entries {
			report.ToolOwned = appendOwnership(report.ToolOwned, migrate.OwnershipRecord{Path: target.Path, Kind: string(entry.ArtifactKind), ID: entry.ArtifactID, Package: entry.Source})
		}
	}
	report.ToolOwned = appendOwnership(report.ToolOwned, migrate.OwnershipRecord{Path: dependency.ProjectFilename, Kind: "state"})
	report.ToolOwned = appendOwnership(report.ToolOwned, migrate.OwnershipRecord{Path: dependency.LockFilename, Kind: "state"})
	report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: "tessl.json", Kind: "manifest"})
	for _, pkg := range inventory.Packages {
		report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: ".tessl/plugins/" + pkg.TesslIdentity, Kind: "package", Package: pkg.TesslIdentity})
		for _, artifact := range pkg.Artifacts {
			for _, native := range artifact.Natives {
				report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: native, Kind: artifact.Kind, ID: artifact.ID, Package: pkg.TesslIdentity})
			}
		}
	}
	for _, record := range inventory.Unmapped {
		report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: record.Path, Kind: "path", Reason: record.Reason})
	}
	for _, record := range inventory.Preserved {
		report.Unmanaged = appendOwnership(report.Unmanaged, migrate.OwnershipRecord{Path: record.Path, Kind: "fragment", Reason: record.Reason})
	}
	if err := addTesslHostOwnership(projectDirectory, inventory, &report); err != nil {
		return migrate.MigrationReport{}, err
	}

	tesslEffective := migrate.FromInventory(inventory)
	var acrEffective migrate.EffectiveSet
	acrNatives := make(map[migrate.EffectiveKey][]string)
	for _, mapping := range mappings {
		locked, ok := lockBySource(state.Lock.Dependencies, mapping.Source)
		if !ok {
			continue
		}
		materialized, cleanup, err := service.resolver.MaterializeLockedAt(ctx, projectDirectory, locked)
		if err != nil {
			return migrate.MigrationReport{}, classifyResolutionError(mapping.Source, err)
		}
		set, effectiveErr := migrate.FromPackage(mapping.From, adapter.Package{Source: mapping.Source, Root: os.DirFS(materialized.Root), Manifest: materialized.Manifest})
		cleanupErr := cleanup()
		if effectiveErr != nil || cleanupErr != nil {
			return migrate.MigrationReport{}, errors.Join(effectiveErr, cleanupErr)
		}
		acrEffective = append(acrEffective, set...)
		for _, item := range set {
			for _, target := range result.Plan.NextLedger.Targets {
				for _, entry := range target.Entries {
					if entry.Source != mapping.Source {
						continue
					}
					if (item.Kind == "rule" && entry.ArtifactKind == realize.ArtifactManagedBlock) || (item.Kind != "rule" && entry.ArtifactID == item.ID) {
						acrNatives[item.EffectiveKey] = appendUniqueString(acrNatives[item.EffectiveKey], target.Path)
					}
				}
			}
		}
	}
	acrEffective = acrEffective.WithNatives(acrNatives)
	report.EffectiveDiffs = migrate.CompareEffective(tesslEffective, acrEffective)
	addCoverageNotes(&report, inventory)
	addFinalizationNotes(&report, inventory)
	addDuplicateEffectNotes(&report, inventory, result.Plan.NextLedger)
	addTransactionNotes(&report, result.Plan.TransactionNotes)
	if err := addGitignoreNotes(projectDirectory, &report); err != nil {
		return migrate.MigrationReport{}, err
	}
	report.FinalizationReady = finalizationReady(inventory, report.EffectiveDiffs)
	if err := validateOwnershipPartition(report); err != nil {
		return migrate.MigrationReport{}, err
	}
	migrate.SortMigrationReport(&report)
	return report, nil
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func validateOwnershipPartition(report migrate.MigrationReport) error {
	seen := make(map[string]string)
	for bucket, records := range map[string][]migrate.OwnershipRecord{
		"toolOwned": report.ToolOwned, "tesslOwned": report.TesslOwned, "unmanaged": report.Unmanaged,
	} {
		for _, record := range records {
			key := record.Path + "\x00" + record.Kind + "\x00" + record.ID + "\x00" + record.Package
			if previous, exists := seen[key]; exists && previous != bucket {
				return fmt.Errorf("ownership partition assigns %s to both %s and %s", record.Path, previous, bucket)
			}
			seen[key] = bucket
		}
	}
	return nil
}

func addTesslHostOwnership(projectDirectory string, inventory migrate.Report, report *migrate.MigrationReport) error {
	candidates := []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md", ".gitignore", ".claude/settings.json", ".codex/config.toml", ".cursor/hooks.json", ".gemini/settings.json", ".github/hooks/tessl.json"}
	for _, candidate := range candidates {
		content, err := os.ReadFile(filepath.Join(projectDirectory, filepath.FromSlash(candidate)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Tessl host %q: %w", candidate, err)
		}
		text := string(content)
		if strings.Contains(text, "<!-- tessl-managed -->") {
			report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: candidate, Kind: "managed-span", ID: "tessl-managed"})
		}
		if strings.Contains(text, "# === Tessl-generated artifacts (managed by ") {
			report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: candidate, Kind: "managed-span", ID: "tessl-gitignore"})
		}
		if strings.Contains(text, "tessl hook run") {
			report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: candidate, Kind: "structured-entry", ID: "tessl-dispatcher"})
		}
		if isNativeHookHost(candidate) && pluginPathHook(content, projectDirectory, inventory) {
			report.TesslOwned = appendOwnership(report.TesslOwned, migrate.OwnershipRecord{Path: candidate, Kind: "structured-entry", ID: "tessl-plugin-path-hook"})
		}
	}
	return nil
}

func isNativeHookHost(path string) bool {
	switch path {
	case ".claude/settings.json", ".codex/config.toml", ".cursor/hooks.json", ".gemini/settings.json", ".github/hooks/tessl.json":
		return true
	default:
		return false
	}
}

func pluginPathHook(content []byte, projectDirectory string, inventory migrate.Report) bool {
	var document map[string]any
	if err := json.Unmarshal(content, &document); err != nil {
		if err := toml.Unmarshal(content, &document); err != nil {
			return false
		}
	}
	return hookTreeHasPluginScript(document["hooks"], projectDirectory, inventory)
}

func hookTreeHasPluginScript(value any, projectDirectory string, inventory migrate.Report) bool {
	switch node := value.(type) {
	case map[string]any:
		if command, ok := node["command"].(string); ok {
			if script := hookScript(command, stringArguments(node["args"])); script != "" && installedPluginScript(script, projectDirectory, inventory) {
				return true
			}
		}
		for _, child := range node {
			if hookTreeHasPluginScript(child, projectDirectory, inventory) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if hookTreeHasPluginScript(child, projectDirectory, inventory) {
				return true
			}
		}
	}
	return false
}

func hookScript(command string, args []string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "bash" || trimmed == "sh" {
		if len(args) == 0 {
			return ""
		}
		return args[0]
	}
	for _, interpreter := range []string{"bash", "sh"} {
		if rest, found := strings.CutPrefix(trimmed, interpreter+" "); found {
			return firstCommandToken(rest)
		}
	}
	return firstCommandToken(trimmed)
}

func firstCommandToken(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	if command[0] == '\'' || command[0] == '"' {
		quote := command[0]
		if end := strings.IndexByte(command[1:], quote); end >= 0 {
			return command[1 : end+1]
		}
		return ""
	}
	if fields := strings.Fields(command); len(fields) != 0 {
		return fields[0]
	}
	return ""
}

func stringArguments(value any) []string {
	if values, ok := value.([]string); ok {
		return append([]string(nil), values...)
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil
		}
		result = append(result, text)
	}
	return result
}

func installedPluginScript(script, projectDirectory string, inventory migrate.Report) bool {
	index := strings.Index(filepath.ToSlash(script), ".tessl/plugins/")
	if index < 0 {
		return false
	}
	relative := filepath.ToSlash(script)[index:]
	for _, pkg := range inventory.Packages {
		prefix := ".tessl/plugins/" + pkg.TesslIdentity + "/"
		if !strings.HasPrefix(relative, prefix) {
			continue
		}
		info, err := os.Lstat(filepath.Join(projectDirectory, filepath.FromSlash(relative)))
		return err == nil && info.Mode().IsRegular()
	}
	return false
}

func addFinalizationNotes(report *migrate.MigrationReport, inventory migrate.Report) {
	for _, record := range inventory.Ambiguous {
		report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "ambiguous", Path: record.Path, Detail: record.Reason})
	}
	for _, record := range inventory.Unsupported {
		report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "unsupported", Path: record.Path, Detail: record.Reason})
	}
	for _, pkg := range inventory.Packages {
		for _, artifact := range pkg.Artifacts {
			if len(artifact.Lossy) != 0 {
				report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "lossy", Path: pkg.TesslIdentity + "/" + artifact.Kind + "/" + artifact.ID, Detail: strings.Join(artifact.Lossy, ", ")})
			}
			if artifact.Classification == "ambiguous" || artifact.Classification == "unsupported" {
				report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: artifact.Classification, Path: pkg.TesslIdentity + "/" + artifact.Kind + "/" + artifact.ID})
			}
		}
	}
}

func selectedAgents(inventory migrate.Report) []string {
	var result []string
	for _, agent := range inventory.Agents {
		if agent.Covered {
			result = append(result, agent.ID)
		}
	}
	sort.Strings(result)
	return result
}

func addCoverageNotes(report *migrate.MigrationReport, inventory migrate.Report) {
	for _, agent := range inventory.Agents {
		if !agent.Covered {
			report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "uncovered-agent", Agent: agent.ID, Artifacts: len(agent.Evidence), Paths: append([]string(nil), agent.Evidence...)})
		}
	}
}

func addDuplicateEffectNotes(report *migrate.MigrationReport, inventory migrate.Report, ledger realize.Ledger) {
	events := make(map[string]string)
	for _, pkg := range inventory.Packages {
		for _, artifact := range pkg.Artifacts {
			if artifact.Kind == "hook" && artifact.Event != "" {
				events[artifact.Event] = "tessl hook run --event=" + artifact.Event
			}
		}
	}
	seen := make(map[string]bool)
	for _, target := range ledger.Targets {
		for _, entry := range target.Entries {
			event := eventFromSourcePath(entry.SourcePath)
			if tesslCommand, ok := events[event]; ok && !seen[event] {
				report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "duplicate-effect", Event: event, Tessl: tesslCommand, ACR: target.Path})
				seen[event] = true
			}
		}
	}
}

func addTransactionNotes(report *migrate.MigrationReport, notes []realize.TransactionNote) {
	for _, note := range notes {
		report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: note.Code, Path: note.Path})
	}
}

func addGitignoreNotes(projectDirectory string, report *migrate.MigrationReport) error {
	return addGitignoreNotesWith(projectDirectory, report, runGitCheckIgnore)
}

type gitCheckIgnoreRunner func(string, string) ([]byte, []byte, error)

func runGitCheckIgnore(projectDirectory, target string) ([]byte, []byte, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := exec.Command("git", "check-ignore", "-v", "--no-index", "--", target)
	command.Dir = projectDirectory
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func addGitignoreNotesWith(projectDirectory string, report *migrate.MigrationReport, checkIgnore gitCheckIgnoreRunner) error {
	if _, err := os.Lstat(filepath.Join(projectDirectory, ".git")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect project Git metadata: %w", err)
	}
	for _, target := range []string{dependency.ProjectFilename, dependency.LockFilename, ".claude/settings.json", ".codex/config.toml", ".cursor/hooks.json"} {
		stdout, stderr, err := checkIgnore(projectDirectory, target)
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue
			}
			return fmt.Errorf("inspect Git ignore status for %q: %w: %s", target, err, strings.TrimSpace(string(stderr)))
		}
		fields := strings.SplitN(strings.TrimSpace(string(stdout)), ":", 4)
		ignoredBy := strings.TrimSpace(string(stdout))
		if len(fields) >= 2 {
			ignoredBy = filepath.ToSlash(fields[0]) + ":" + fields[1]
		}
		report.Notes = append(report.Notes, migrate.CoexistenceNote{Code: "gitignored_state", Path: target, IgnoredBy: ignoredBy})
	}
	return nil
}

func hasLossy(inventory migrate.Report) bool {
	for _, pkg := range inventory.Packages {
		for _, artifact := range pkg.Artifacts {
			if len(artifact.Lossy) != 0 {
				return true
			}
		}
	}
	return false
}

func hasUncovered(inventory migrate.Report) bool {
	for _, agent := range inventory.Agents {
		if !agent.Covered {
			return true
		}
	}
	return false
}

func hasAmbiguousArtifact(inventory migrate.Report) bool {
	return hasArtifactClassification(inventory, "ambiguous")
}

func hasArtifactClassification(inventory migrate.Report, classification string) bool {
	for _, pkg := range inventory.Packages {
		for _, artifact := range pkg.Artifacts {
			if artifact.Classification == classification {
				return true
			}
		}
	}
	return false
}

func finalizationReady(inventory migrate.Report, diffs []migrate.EffectiveDiff) bool {
	return len(diffs) == 0 && len(inventory.Ambiguous) == 0 && !hasAmbiguousArtifact(inventory) && !hasLossy(inventory) && !hasUncovered(inventory)
}

func eventFromSourcePath(sourcePath string) string {
	base := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
	return strings.ReplaceAll(base, "_", "-")
}

func appendOwnership(records []migrate.OwnershipRecord, record migrate.OwnershipRecord) []migrate.OwnershipRecord {
	for _, existing := range records {
		if existing == record {
			return records
		}
	}
	return append(records, record)
}

func declarationBySource(values []dependency.Declaration, source string) (dependency.Declaration, bool) {
	for _, value := range values {
		if value.Source == source {
			return value, true
		}
	}
	return dependency.Declaration{}, false
}

func lockBySource(values []dependency.LockedDependency, source string) (dependency.LockedDependency, bool) {
	for _, value := range values {
		if value.Source == source {
			return value, true
		}
	}
	return dependency.LockedDependency{}, false
}

func sameDeclarations(left, right []dependency.Declaration) bool {
	if len(left) != len(right) {
		return false
	}
	for _, declaration := range left {
		other, ok := declarationBySource(right, declaration.Source)
		if !ok || declaration.Requested != other.Requested {
			return false
		}
	}
	return true
}

func sameLocks(left, right []dependency.LockedDependency) bool {
	if len(left) != len(right) {
		return false
	}
	for _, locked := range left {
		other, ok := lockBySource(right, locked.Source)
		if !ok || !reflect.DeepEqual(locked, other) {
			return false
		}
	}
	return true
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func isNotFound(err error) bool {
	var remote *dependency.RemoteError
	return errors.As(err, &remote) && remote.StatusCode == 404
}

func classifyResolutionError(source string, err error) error {
	if strings.Contains(err.Error(), "agent-plugin.yaml") || strings.Contains(err.Error(), "package manifest") {
		return namedError("source_not_a_package", fmt.Sprintf("%s is not an ACR package; producer migration #11 and a package release #9 are required: %v", source, err), err)
	}
	return err
}

func namedError(code, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}
