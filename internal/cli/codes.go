package cli

// Codes reused across application packages have named declarations so their
// machine-readable values share one stable contract.
const (
	CodeAmbiguousTesslVersion      = "ambiguous_tessl_version"
	CodeDependencyHoldResumable    = "dependency_hold_resumable"
	CodeEffectiveMismatch          = "effective_mismatch"
	CodeFinalizationConflict       = "finalization_conflict"
	CodeFinalizationFailed         = "finalization_failed"
	CodeMappingConflict            = "mapping_conflict"
	CodePendingTransaction         = "pending_transaction"
	CodeProjectStateConflict       = "project_state_conflict"
	CodeRecoveryConflict           = "recovery_conflict"
	CodeSourceNotAPackage          = "source_not_a_package"
	CodeTesslManifestAbsent        = "tessl_manifest_absent"
	CodeTesslVersionUnavailable    = "tessl_version_unavailable"
	CodeTransactionBusy            = "transaction_busy"
	CodeTransactionLockUnavailable = "transaction_lock_unavailable"
	CodeUnmappedPackage            = "unmapped_package"
	CodeUnsupportedJournalVersion  = "unsupported_journal_version"
	CodeVendorCollision            = "vendor_collision"
	CodeVendorEscape               = "vendor_escape"
)

// RefusalCodes is the complete, stable set of machine-readable codes that can
// describe a non-successful operation. Each code has a command remedy in the
// troubleshooting reference.
var RefusalCodes = []string{
	"adapter_realization_failed",
	"agent_widening",
	"ambiguous_manifest",
	"ambiguous_tag",
	CodeAmbiguousTesslVersion,
	"config_conflict",
	CodeDependencyNotDeclared,
	"dependency_operation_failed",
	"dirty_worktree",
	"downgrade_cancelled",
	CodeDowngradeChoiceRequired,
	"duplicate_activation_path",
	"duplicate_artifact_id",
	"duplicate_config_entry",
	"duplicate_include",
	CodeEffectiveMismatch,
	"finalization_blocked",
	CodeFinalizationConflict,
	CodeFinalizationFailed,
	"foreign_draft_release",
	"freshness_auth",
	"freshness_conflict",
	"freshness_lock_release_failed",
	"freshness_offline",
	"freshness_state_unwritable",
	"freshness_update_failed",
	"git_access_failed",
	"include_cycle",
	"invalid_artifact_id",
	"invalid_artifact_type",
	"invalid_executable_mode",
	"invalid_include",
	"invalid_native_event",
	"invalid_package_name",
	"invalid_path",
	"invalid_rule_activation",
	"invalid_skill_tree",
	"invalid_source",
	"invalid_version",
	"json_encoding_failed",
	"malformed_frontmatter",
	"manifest_conflict",
	CodeMappingConflict,
	"mapping_file_invalid",
	"marker_conflict",
	"migrate_failed",
	"no_agent_selected",
	"no_artifacts",
	"no_publishable_tag",
	"not_implemented",
	"operation_failed",
	"output_failed",
	"ownership_conflict",
	"path_not_found",
	CodePendingTransaction,
	CodeProjectStateConflict,
	"publish_failed",
	"realization_changes",
	"realization_conflict",
	"realization_failed",
	CodeRecoveryConflict,
	"release_already_exists",
	"release_upload_failed",
	"remaining_packages_unavailable",
	"required",
	"setup_cancelled",
	"setup_failed",
	CodeSourceNotAPackage,
	"tag_commit_mismatch",
	"tag_not_pushed",
	"tag_version_mismatch",
	CodeTesslManifestAbsent,
	"tessl_owned_target",
	CodeTesslVersionUnavailable,
	CodeTransactionBusy,
	CodeTransactionLockUnavailable,
	"unknown_field",
	"unmapped_field",
	CodeUnmappedPackage,
	"unpublishable_content",
	"unpublishable_path",
	"unresolved_include",
	"unsupported_adapter_capability",
	"unsupported_hook_event",
	CodeUnsupportedJournalVersion,
	"unsupported_schema_version",
	"usage",
	CodeVendorCollision,
	CodeVendorEscape,
	"vendor_source_read_only",
}

// NoticeCodes is the complete, stable set of machine-readable codes that
// describe an exit-zero observation.
var NoticeCodes = []string{
	"ambiguous",
	CodeDependencyHoldResumable,
	"duplicate-effect",
	"freshness_busy",
	"freshness_outdated",
	"gitignored_state",
	"lossy",
	"no-version-control",
	"restart_required",
	"shared_file_requires_commit",
	"stale_transaction_staging",
	"tessl_not_installed",
	"uncovered-agent",
	"unsupported",
}
