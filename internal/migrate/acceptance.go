package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AcceptanceSchemaVersion versions the reviewed-change evidence bundle. It is
// hashed into the token, so a bundle produced under a different version can
// never satisfy this one.
const AcceptanceSchemaVersion = 1

// AcceptanceTokenPrefix labels an acceptance token so an operator can tell it
// apart from a commit, a content hash, or a package version.
const AcceptanceTokenPrefix = "acr-accept-1:"

// AcceptedBinding ties one acceptance to the packages it was reviewed
// against: the installed Tessl package's own effective content, and the
// replacement's source, requested ref, resolved commit, and resolved content.
// Any change on either side yields a different token.
type AcceptedBinding struct {
	From         string `json:"from"`
	TesslVersion string `json:"tesslVersion,omitempty"`
	TesslDigest  string `json:"tesslDigest"`
	Source       string `json:"source"`
	Requested    string `json:"requested,omitempty"`
	Commit       string `json:"commit,omitempty"`
	ContentHash  string `json:"contentHash"`
}

// AcceptedChange is one reviewed difference between the installed Tessl
// artifact and its ACR replacement. It records the difference, never an
// equivalence: the artifact stays in effectiveDiffs and is reported as
// accepted, not as identical.
type AcceptedChange struct {
	Package     string `json:"package"`
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Reason      string `json:"reason"`
	Detail      string `json:"detail,omitempty"`
	TesslDigest string `json:"tesslDigest,omitempty"`
	ACRDigest   string `json:"acrDigest,omitempty"`
}

// Acceptance is the evidence bundle a finalization preview issues and
// --accept-reviewed-changes replays. Token is empty when the project has no
// reviewable difference, which is what makes an unsolicited token a mismatch
// rather than a no-op.
type Acceptance struct {
	SchemaVersion int               `json:"schemaVersion"`
	Token         string            `json:"token"`
	Bindings      []AcceptedBinding `json:"bindings"`
	Changes       []AcceptedChange  `json:"changes"`
}

// AcceptableDiff reports whether one effective difference describes a
// reviewed change to an artifact the replacement actually carries.
//
// missing-in-acr is never acceptable: the replacement has no counterpart to
// review, so accepting it would retire Tessl output against evidence that
// does not exist. It keeps blocking, and so does every artifact ACR could not
// classify.
func AcceptableDiff(reason string) bool {
	switch reason {
	case DiffBody, DiffActivation, DiffEvent, DiffLossy, DiffMissingInTessl:
		return true
	default:
		return false
	}
}

// AcceptedChanges selects the reviewable differences of one comparison.
//
// An artifact classified ambiguous is excluded: acceptance covers a reviewed
// change, never an unresolved ownership question.
func AcceptedChanges(report Report, diffs []EffectiveDiff) []AcceptedChange {
	artifacts := artifactsByKey(report)
	changes := make([]AcceptedChange, 0, len(diffs))
	for _, diff := range diffs {
		if !AcceptableDiff(diff.Reason) {
			continue
		}
		artifact, installed := artifacts[acceptedKey(diff.Package, diff.Kind, diff.ID)]
		if !installed && diff.Reason != DiffMissingInTessl {
			continue
		}
		if installed && artifact.Classification == classAmbiguous {
			continue
		}
		change := AcceptedChange{
			Package: diff.Package, Kind: diff.Kind, ID: diff.ID, Reason: diff.Reason,
			TesslDigest: diff.TesslDigest, ACRDigest: diff.ACRDigest,
		}
		if len(artifact.Lossy) != 0 {
			change.Detail = strings.Join(artifact.Lossy, ", ")
		}
		changes = append(changes, change)
	}
	return changes
}

// NewAcceptance canonicalizes one evidence bundle and stamps its token.
func NewAcceptance(bindings []AcceptedBinding, changes []AcceptedChange) (Acceptance, error) {
	acceptance := Acceptance{
		SchemaVersion: AcceptanceSchemaVersion,
		Bindings:      append([]AcceptedBinding{}, bindings...),
		Changes:       append([]AcceptedChange{}, changes...),
	}
	sort.Slice(acceptance.Bindings, func(left, right int) bool {
		return acceptance.Bindings[left].From < acceptance.Bindings[right].From
	})
	sort.Slice(acceptance.Changes, func(left, right int) bool {
		return acceptedChangeOrder(acceptance.Changes[left]) < acceptedChangeOrder(acceptance.Changes[right])
	})
	if len(acceptance.Changes) == 0 {
		return acceptance, nil
	}
	token, err := acceptanceToken(acceptance)
	if err != nil {
		return Acceptance{}, err
	}
	acceptance.Token = token
	return acceptance, nil
}

// Grants returns the set one acceptance authorizes. A bundle with no token
// grants nothing, whatever it lists.
func (acceptance Acceptance) Grants() AcceptedSet {
	if acceptance.Token == "" {
		return AcceptedSet{}
	}
	covered := make(map[string]bool, len(acceptance.Changes))
	for _, change := range acceptance.Changes {
		covered[acceptedKey(change.Package, change.Kind, change.ID)] = true
	}
	return AcceptedSet{covered: covered}
}

// AcceptedSet answers, per artifact, whether an operator accepted its
// reviewed change. The zero value accepts nothing, which is what every run
// without a matching token plans against.
type AcceptedSet struct {
	covered map[string]bool
}

// Covers reports whether the acceptance names one artifact.
func (set AcceptedSet) Covers(pkg, kind, id string) bool {
	return set.covered[acceptedKey(pkg, kind, id)]
}

// Empty reports a set that accepts nothing.
func (set AcceptedSet) Empty() bool { return len(set.covered) == 0 }

// installedArtifactEvidence is the canonical encoding of one installed
// artifact's reported evidence. Every field is serialized structurally, so no
// value an artifact can carry — a path holding a comma, a lossy entry holding
// the discarded applyTo clause — can be read as a different field or as a
// different array shape.
//
// Natives are deliberately absent. A native appearing or disappearing changes
// what ACR would realize, which the pending-coexistence and uncovered-agent
// gates refuse independently of acceptance.
type installedArtifactEvidence struct {
	Kind            string   `json:"kind"`
	ID              string   `json:"id"`
	Classification  string   `json:"classification"`
	Digest          string   `json:"digest"`
	ActivationMode  string   `json:"activationMode"`
	ActivationPaths []string `json:"activationPaths"`
	Event           string   `json:"event"`
	Lossy           []string `json:"lossy"`
}

// PackageEffectiveDigest fingerprints one installed Tessl package's complete
// reported evidence, so an acceptance is bound to the old package's actual
// classification, body, activation, event and dropped behaviour, and not only
// to the differences it happened to produce.
func PackageEffectiveDigest(report Report, identity string) (string, error) {
	artifacts := make([]installedArtifactEvidence, 0)
	for _, pkg := range report.Packages {
		if pkg.TesslIdentity != identity {
			continue
		}
		for _, artifact := range pkg.Artifacts {
			evidence := installedArtifactEvidence{
				Kind: artifact.Kind, ID: artifact.ID, Classification: artifact.Classification,
				Digest: artifact.Digest, Event: artifact.Event,
				ActivationPaths: []string{}, Lossy: []string{},
			}
			if artifact.Activation != nil {
				evidence.ActivationMode = artifact.Activation.Mode
				evidence.ActivationPaths = append(evidence.ActivationPaths, artifact.Activation.Paths...)
				sort.Strings(evidence.ActivationPaths)
			}
			evidence.Lossy = append(evidence.Lossy, artifact.Lossy...)
			sort.Strings(evidence.Lossy)
			artifacts = append(artifacts, evidence)
		}
	}
	sort.Slice(artifacts, func(left, right int) bool {
		if artifacts[left].Kind != artifacts[right].Kind {
			return artifacts[left].Kind < artifacts[right].Kind
		}
		return artifacts[left].ID < artifacts[right].ID
	})
	encoded, err := json.Marshal(artifacts)
	if err != nil {
		return "", fmt.Errorf("encode installed package evidence: %w", err)
	}
	return contentDigest(encoded), nil
}

func acceptanceToken(acceptance Acceptance) (string, error) {
	payload, err := json.Marshal(struct {
		SchemaVersion int               `json:"schemaVersion"`
		Bindings      []AcceptedBinding `json:"bindings"`
		Changes       []AcceptedChange  `json:"changes"`
	}{SchemaVersion: acceptance.SchemaVersion, Bindings: acceptance.Bindings, Changes: acceptance.Changes})
	if err != nil {
		return "", fmt.Errorf("encode reviewed-change acceptance: %w", err)
	}
	sum := sha256.Sum256(payload)
	return AcceptanceTokenPrefix + hex.EncodeToString(sum[:]), nil
}

func artifactsByKey(report Report) map[string]ArtifactReport {
	artifacts := make(map[string]ArtifactReport)
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			artifacts[acceptedKey(pkg.TesslIdentity, artifact.Kind, artifact.ID)] = artifact
		}
	}
	return artifacts
}

func acceptedChangeOrder(change AcceptedChange) string {
	return change.Package + "\x00" + change.Kind + "\x00" + change.ID
}

func acceptedKey(pkg, kind, id string) string {
	return pkg + "\x00" + kind + "\x00" + id
}
