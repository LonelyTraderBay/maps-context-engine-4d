// Package profiles contains read-only domain review prompts. It does not
// define business rules or execute trading, commerce, or provider workflows.
package profiles

// ID identifies one built-in domain review profile.
type ID string

const (
	Trading  ID = "trading"
	Commerce ID = "commerce"
)

// CandidateMarkers may suggest which profile to inspect, but a marker by
// itself is never evidence that a workflow or policy exists.
type Profile struct {
	ID               ID
	AcceptanceCase   string
	CandidateMarkers []string
	// EntityCategories are concepts to inspect, not claims that the repository
	// contains those entities or state machines.
	EntityCategories []string
	// ReviewTopics are independent question areas; their order is not a workflow.
	ReviewTopics []string
	// Obligations are review questions, not accepted business rules.
	Obligations []Obligation
}

// Obligation is a question that must be answered from a source or reported as
// a gap. MissingEvidence describes the safe result when no source is found.
type Obligation struct {
	ID              string
	Question        string
	MissingEvidence string
}

var builtIns = [...]Profile{
	{
		ID:             Trading,
		AcceptanceCase: "C065",
		CandidateMarkers: []string{
			"order", "reserve", "retry", "partial fill", "cancel", "ledger",
		},
		EntityCategories: []string{"order", "account", "position", "ledger"},
		ReviewTopics:     []string{"reserve", "retry", "partial fill", "cancel", "ledger"},
		Obligations: []Obligation{
			{
				ID:              "reserve-boundary",
				Question:        "Which source defines the owner, scope, and transaction boundary for reserve and release?",
				MissingEvidence: "Leave the reserve/release boundary unknown; do not infer it from a status name or nearby code.",
			},
			{
				ID:              "retry-side-effects",
				Question:        "Which source defines retry identity, idempotency scope, and possible repeated side effects?",
				MissingEvidence: "Report a gap; do not claim exactly-once behavior or invent a retry schedule.",
			},
			{
				ID:              "partial-fill-cancel-ledger",
				Question:        "Which evidence connects partial fills, cancellation of remaining work, late events, and ledger reconciliation?",
				MissingEvidence: "Keep unlinked transitions and ledger obligations as gaps; do not infer cumulative quantities or final state.",
			},
			{
				ID:              "money-precision",
				Question:        "Which declared source specifies currency or unit, amount precision, and rounding?",
				MissingEvidence: "Keep precision and rounding unknown; do not hardcode a scale or perform amount arithmetic.",
			},
			{
				ID:              "settlement-finality",
				Question:        "Which source defines settlement and finality boundaries for the observed workflow?",
				MissingEvidence: "Keep finality unknown; a status value, receipt, or transaction identifier is not proof by itself.",
			},
		},
	},
	{
		ID:             Commerce,
		AcceptanceCase: "C066",
		CandidateMarkers: []string{
			"order", "payment", "webhook", "refund", "stock",
		},
		EntityCategories: []string{"order", "payment", "stock"},
		ReviewTopics:     []string{"duplicate webhook", "out-of-order webhook", "partial refund"},
		Obligations: []Obligation{
			{
				ID:              "separate-state-domains",
				Question:        "Which source independently defines order, payment, and stock lifecycle state?",
				MissingEvidence: "Keep order, payment, and stock evidence separate; do not merge similarly named states.",
			},
			{
				ID:              "webhook-delivery",
				Question:        "Which provider contract defines webhook identity, duplicate delivery, ordering, and retry behavior?",
				MissingEvidence: "Report unknown delivery semantics; do not infer ordering or exactly-once behavior.",
			},
			{
				ID:              "partial-refund",
				Question:        "Which evidence distinguishes a partial refund from a full refund and identifies the affected state domains?",
				MissingEvidence: "Leave refund state and its effects unknown where sources do not define them.",
			},
			{
				ID:              "money-precision-finality",
				Question:        "Which declared source specifies currency or unit, amount precision, rounding, and refund finality?",
				MissingEvidence: "Keep precision, rounding, and finality unknown; do not infer them from provider or field names.",
			},
			{
				ID:              "known-impacted-consumers",
				Question:        "Which consumers are linked to the affected state by retained evidence?",
				MissingEvidence: "Return only evidence-linked consumers and preserve missing causal links as gaps.",
			},
		},
	},
}

// BuiltIns returns independent copies of the built-in review profiles.
func BuiltIns() []Profile {
	profiles := make([]Profile, len(builtIns))
	for index, profile := range builtIns {
		profiles[index] = clone(profile)
	}
	return profiles
}

// Lookup returns an independent copy of a built-in profile.
func Lookup(id ID) (Profile, bool) {
	for _, profile := range builtIns {
		if profile.ID == id {
			return clone(profile), true
		}
	}
	return Profile{}, false
}

func clone(profile Profile) Profile {
	profile.CandidateMarkers = append([]string(nil), profile.CandidateMarkers...)
	profile.EntityCategories = append([]string(nil), profile.EntityCategories...)
	profile.ReviewTopics = append([]string(nil), profile.ReviewTopics...)
	profile.Obligations = append([]Obligation(nil), profile.Obligations...)
	return profile
}
