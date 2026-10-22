package profiles

import "testing"

func TestBuiltInsCoverTradingAndCommerceCases(t *testing.T) {
	profiles := BuiltIns()
	if len(profiles) != 2 {
		t.Fatalf("built-in profile count = %d, want 2", len(profiles))
	}
	if profiles[0].ID != Trading || profiles[0].AcceptanceCase != "C065" {
		t.Fatalf("trading profile identity = %+v", profiles[0])
	}
	if profiles[1].ID != Commerce || profiles[1].AcceptanceCase != "C066" {
		t.Fatalf("commerce profile identity = %+v", profiles[1])
	}
}

func TestTradingProfileKeepsTransactionalAndEconomicUnknownsExplicit(t *testing.T) {
	profile, ok := Lookup(Trading)
	if !ok {
		t.Fatal("trading profile not found")
	}
	for _, topic := range []string{"reserve", "retry", "partial fill", "cancel", "ledger"} {
		if !contains(profile.ReviewTopics, topic) {
			t.Errorf("trading profile is missing workflow topic %q", topic)
		}
	}
	for _, id := range []string{"reserve-boundary", "retry-side-effects", "partial-fill-cancel-ledger", "money-precision", "settlement-finality"} {
		if !hasObligation(profile.Obligations, id) {
			t.Errorf("trading profile is missing obligation %q", id)
		}
	}
	if got := obligation(profile.Obligations, "money-precision").MissingEvidence; got == "" {
		t.Fatal("missing money precision must remain an explicit gap")
	}
	if got := obligation(profile.Obligations, "settlement-finality").MissingEvidence; got == "" {
		t.Fatal("missing settlement finality must remain an explicit gap")
	}
}

func TestCommerceProfileSeparatesStateDomainsAndWebhookObligations(t *testing.T) {
	profile, ok := Lookup(Commerce)
	if !ok {
		t.Fatal("commerce profile not found")
	}
	if !sameStrings(profile.EntityCategories, []string{"order", "payment", "stock"}) {
		t.Fatalf("commerce entity categories = %v, want distinct order/payment/stock", profile.EntityCategories)
	}
	for _, id := range []string{"separate-state-domains", "webhook-delivery", "partial-refund", "money-precision-finality", "known-impacted-consumers"} {
		if !hasObligation(profile.Obligations, id) {
			t.Errorf("commerce profile is missing obligation %q", id)
		}
	}
	if !contains(profile.ReviewTopics, "duplicate webhook") || !contains(profile.ReviewTopics, "out-of-order webhook") || !contains(profile.ReviewTopics, "partial refund") {
		t.Fatalf("commerce review topics omit C066 scenarios: %v", profile.ReviewTopics)
	}
}

func TestReturnedProfilesCannotMutateBuiltInCatalog(t *testing.T) {
	profile, ok := Lookup(Trading)
	if !ok {
		t.Fatal("trading profile not found")
	}
	profile.CandidateMarkers[0] = "changed"
	profile.EntityCategories[0] = "changed"
	profile.ReviewTopics[0] = "changed"
	profile.Obligations[0].ID = "changed"
	profile.Obligations[0].Question = "changed"

	again, ok := Lookup(Trading)
	if !ok {
		t.Fatal("trading profile not found on second lookup")
	}
	if again.CandidateMarkers[0] == "changed" || again.EntityCategories[0] == "changed" || again.ReviewTopics[0] == "changed" || again.Obligations[0].ID == "changed" || again.Obligations[0].Question == "changed" {
		t.Fatal("returned profile mutated the built-in catalog")
	}
}

func TestLookupRejectsUnknownProfile(t *testing.T) {
	if _, ok := Lookup(ID("unknown")); ok {
		t.Fatal("unknown profile unexpectedly resolved")
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func hasObligation(obligations []Obligation, id string) bool {
	for _, item := range obligations {
		if item.ID == id {
			return item.Question != "" && item.MissingEvidence != ""
		}
	}
	return false
}

func obligation(obligations []Obligation, id string) Obligation {
	for _, item := range obligations {
		if item.ID == id {
			return item
		}
	}
	return Obligation{}
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
