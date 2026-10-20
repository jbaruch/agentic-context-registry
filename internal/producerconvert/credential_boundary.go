package producerconvert

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

const credentialContract = "acr-credential-boundary/v1"

type RunCredentialBoundary struct {
	Contract            string `json:"contract"`
	AuthInspected       bool   `json:"authInspected"`
	ProposalChecked     bool   `json:"proposalChecked"`
	ReportSanitized     bool   `json:"reportSanitized"`
	IsolatedHomeRemoved bool   `json:"isolatedHomeRemoved"`
	RefreshObserved     bool   `json:"refreshObserved"`
}

type PlanCredentialBoundary struct {
	Contract           string `json:"contract"`
	PlanChecked        bool   `json:"planChecked"`
	ApplicationChecked bool   `json:"applicationChecked"`
	ReportSanitized    bool   `json:"reportSanitized"`
}

// This unexported per-migration union survives private-home removal. It is never
// serialized or persisted and matches only literal values actually observed.
type credentialGuard []string

var errCredentialOutput = errors.New("Codex proposal or materialized output contains a known credential; no source changes were made")

func (g credentialGuard) contains(s string) bool {
	for _, secret := range g {
		if secret != "" && strings.Contains(s, secret) {
			return true
		}
	}
	return false
}

func (g credentialGuard) check(value any) error {
	var visit func(reflect.Value) bool
	visit = func(v reflect.Value) bool {
		switch v.Kind() {
		case reflect.String:
			return g.contains(v.String())
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				return visit(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() && visit(v.Field(i)) {
					return true
				}
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				if visit(v.Index(i)) {
					return true
				}
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				if visit(iter.Key()) || visit(iter.Value()) {
					return true
				}
			}
		}
		return false
	}
	if visit(reflect.ValueOf(value)) {
		return errCredentialOutput
	}
	return nil
}

func (g credentialGuard) checkPlan(p Plan) error {
	for name, state := range p.after {
		if g.contains(name) || g.contains(string(state.Content)) || g.contains(state.Link) {
			return errCredentialOutput
		}
	}
	if g.contains(string(p.receipt)) {
		return errCredentialOutput
	}
	// Receipt JSON may escape credential characters. Inspect decoded strings too.
	var receipt any
	if err := json.Unmarshal(p.receipt, &receipt); err != nil {
		return err
	}
	return g.check(receipt)
}

func (g credentialGuard) sanitizeReport(report Report) Report {
	// Copy the presentation tree, keeping private operational buffers untouched.
	data, err := json.Marshal(report)
	if err != nil {
		panic(err)
	} // Report is a closed tree of JSON-compatible primitives.
	var copy Report
	if err := json.Unmarshal(data, &copy); err != nil {
		panic(err)
	}
	var visit func(reflect.Value)
	visit = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			if v.CanSet() {
				v.SetString(redactCodexSecrets(v.String(), g))
			}
		case reflect.Pointer:
			if !v.IsNil() {
				visit(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					visit(v.Field(i))
				}
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				visit(v.Index(i))
			}
		}
	}
	visit(reflect.ValueOf(&copy).Elem())
	return copy
}

func (g credentialGuard) sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	if message := redactCodexSecrets(err.Error(), g); message != err.Error() {
		var refusal *Error
		if errors.As(err, &refusal) {
			return refuse(refusal.Code, redactCodexSecrets(refusal.Path, g), redactCodexSecrets(refusal.Reason, g))
		}
		return errors.New(message)
	}
	return err
}
