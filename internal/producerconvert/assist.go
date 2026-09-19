package producerconvert

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

//go:embed semantic-prompt.txt
var semanticPrompt string

type providerCall func(context.Context, string, string) (proposal, AgentRun, error)

func prepareAssisted(ctx context.Context, options Options) (Plan, error) {
	return prepareWithProvider(ctx, options, runProvider)
}

func prepareWithProvider(ctx context.Context, options Options, provider providerCall) (plan Plan, err error) {
	var guard credentialGuard
	defer func() {
		if guard != nil {
			plan.guard = guard
			plan.Report = guard.sanitizeReport(plan.Report)
			if plan.Report.CredentialBoundary != nil {
				plan.Report.CredentialBoundary.ReportSanitized = true
			}
			err = guard.sanitizeError(err)
		}
	}()
	plan, err = prepareDeterministic(options, options.Agent != "")
	if err == nil {
		return plan, nil
	}
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || plan.Report.Package == "" {
		return plan, err
	}
	for _, block := range plan.Report.Blockers {
		if strings.HasPrefix(block.Path, ".github/") && !supportedDeliveryFile(plan.before, block.Path) {
			return plan, refuse("unsupported_semantic_conversion", block.Path, "unsupported delivery format contains Tessl operations; retain this read-only policy file until its format has a supported conversion")
		}
		if strings.Contains(block.Reason, "competes") || strings.Contains(block.Reason, "ambiguous") {
			return plan, err
		}
	}
	if options.Agent != "claude" && options.Agent != "codex" {
		return plan, refuse("agent_unavailable", "--agent", "select --agent codex or --agent claude explicitly")
	}
	input := semanticInput{Selected: plan.options.PackageRoot, Package: plan.Report.Package, Version: plan.Report.Version, Blockers: plan.Report.Blockers}
	for _, name := range sortedPaths(plan.before) {
		state := plan.before[name]
		if state.Directory || state.Link != "" || state.Digest == "" {
			continue
		}
		if !utf8.Valid(state.Content) {
			continue
		}
		input.Files = append(input.Files, inputFile{name, state.Digest, state.Mode, editable(plan, name), string(state.Content)})
	}
	requests, e := scopeInputs(input)
	if e != nil {
		return plan, e
	}
	if len(requests) == 0 {
		return plan, refuse("unsupported_semantic_conversion", plan.options.PackageRoot, "no owned editable semantic input; correct unsupported references in the source before retrying")
	}

	plan.Report.Notes = append(plan.Report.Notes, "Semantic proposals use the explicitly selected CLI and configured account. ACR validates proposals before writing. ACR-only removes Tessl-only paid scoring; ACR validation is not a replacement score.")
	if len(requests) > 1 {
		plan.Report.Notes = append(plan.Report.Notes, "Large input is split into runtime, instruction and delivery proposals; their combined result is validated and committed in one transaction.")
	}
	original := plan
	previous := proposal{}
	feedback := ""
	cached := make([]proposal, len(requests))
	proposalRuns := make([]int, len(requests))
	retryFrom := 0
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		combined := proposal{}
		for index, input := range requests {
			if index < retryFrom {
				combined.Edits = append(combined.Edits, cached[index].Edits...)
				combined.PolicyChanges = append(combined.PolicyChanges, cached[index].PolicyChanges...)
				continue
			}
			request, e := proposalRequest(input, previous, combined, feedback)
			if e != nil {
				return plan, e
			}
			proposed, run, callErr := provider(ctx, options.Agent, request)
			if run.CredentialBoundary != nil && guard == nil {
				guard = credentialGuard{}
			}
			guard = append(guard, run.guard...)
			run.Scope = input.Scope
			plan.Report.AgentRuns = append(plan.Report.AgentRuns, run)
			if callErr != nil {
				return plan, refuse("agent_failed", "--agent", callErr.Error())
			}
			if err := guard.check(combined); err != nil {
				return plan, err
			}
			if err := guard.check(proposed); err != nil {
				return plan, err
			}
			if input.Scope != "" {
				for _, edit := range proposed.Edits {
					if semanticScope(edit.Path) != input.Scope {
						return plan, refuse("invalid_agent_proposal", edit.Path, "proposal edited outside its assigned "+input.Scope+" scope")
					}
				}
			}
			proposalRuns[index] = len(plan.Report.AgentRuns)
			cached[index] = proposed
			combined.Edits = append(combined.Edits, proposed.Edits...)
			combined.PolicyChanges = append(combined.PolicyChanges, proposed.PolicyChanges...)
		}
		if err := guard.check(combined); err != nil {
			return plan, err
		}
		original.guard = guard
		next, validationErr := validateProposal(ctx, original, combined)
		if errors.Is(validationErr, errCredentialOutput) {
			return plan, validationErr
		}
		attemptNote := fmt.Sprintf("ACR combined validation attempt %d (proposal runs %v)", attempt+1, proposalRuns)
		if validationErr == nil {
			if guard != nil {
				if err := guard.checkPlan(next); err != nil {
					return plan, err
				}
				next.Report.CredentialBoundary = &PlanCredentialBoundary{Contract: credentialContract, PlanChecked: true}
			}
			plan.Report.Notes = append(plan.Report.Notes, attemptNote+": passed.")
			next.Report.AgentRuns = plan.Report.AgentRuns
			next.Report.Notes = append(next.Report.Notes, plan.Report.Notes...)
			return next, nil
		}
		attemptNote += ": " + validationErr.Error()
		plan.Report.Notes = append(plan.Report.Notes, attemptNote)
		if !onlySemanticValidation(validationErr) {
			return plan, validationErr
		}
		var attributable bool
		retryFrom, attributable = firstAffectedScope(requests, combined, validationErr)
		if attributable {
			run := &plan.Report.AgentRuns[proposalRuns[retryFrom]-1]
			if run.Failure != "" {
				run.Failure += "\n"
			}
			run.Failure += attemptNote
			if onlySemanticValidation(validationErr) && run.CredentialBoundary != nil && run.CredentialBoundary.AuthInspected && run.CredentialBoundary.ProposalChecked && run.CredentialBoundary.ReportSanitized && run.CredentialBoundary.IsolatedHomeRemoved {
				run.FailureKind = "semantic_validation"
			}
		}
		err = refuse("invalid_agent_proposal", "--agent", validationErr.Error())
		previous, feedback = combined, validationErr.Error()
	}
	return plan, err
}

// Existing skill trees are the schema-1 support-file transport shared by all
// adapters. Keep originals intact and carry notices byte-for-byte in one skill.
func (p *Plan) addSupport(_ *os.Root, value manifest.Manifest) error {
	if len(value.Artifacts.Skills) == 0 {
		return refuse("unsupported_support_files", manifest.Filename, "semantic packages need a skill tree for portable metadata and notices")
	}
	metadata, err := json.MarshalIndent(struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Source  string `json:"source"`
	}{value.Name, value.Version, "github:" + value.Name}, "", "  ")
	if err != nil {
		return err
	}
	metadata = append(metadata, '\n')
	add := func(name string, data []byte, mode uint32) error {
		if old, exists := p.after[name]; exists {
			if !bytes.Equal(old.Content, data) || old.Mode != mode {
				return refuse("support_collision", name, "portable support file collides with existing content")
			}
			return nil
		}
		p.change(name, data, mode)
		return nil
	}
	for _, skill := range value.Artifacts.Skills {
		if err := add(path.Join(skill.Path, ".acr-package.json"), metadata, 0o644); err != nil {
			return err
		}
	}
	for _, name := range sortedPaths(p.before) {
		state := p.before[name]
		if !distributionNotice(name) || state.Directory {
			continue
		}
		published := false
		for _, skill := range value.Artifacts.Skills {
			if within(skill.Path, name) {
				published = true
			}
		}
		if !published {
			if state.Link != "" || state.Digest == "" {
				return refuse("unsafe_path", name, "notice must be a readable regular file")
			}
			target := path.Join(value.Artifacts.Skills[0].Path, path.Base(name)+".acr-"+strings.TrimPrefix(state.Digest, "sha256:")[:12])
			if err := add(target, state.Content, state.Mode); err != nil {
				return err
			}
			p.Report.Notes = append(p.Report.Notes, fmt.Sprintf("Preserved %s unchanged; publication also carries identical bytes at %s.", name, target))
		}
	}
	return nil
}
