package contextpack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestSelectBudgetKeepsItemsWholeAndCountsRequiredPending(t *testing.T) {
	largePayload := `{"id":"` + strings.Repeat("x", 24) + `"}`
	items := []BudgetCandidate{
		budgetCandidate("req-small", true, SliceDeclaration, `{"id":"a"}`),
		budgetCandidate("req-large", true, SliceContract, largePayload),
		budgetCandidate("optional", false, SliceNeighbor, `{"id":"b"}`),
	}
	selection, err := SelectBudget(context.Background(), items, BudgetOptions{
		BaseBytes: 100, MaxBytes: 150, MaxItems: 10, DeliveryMultiplier: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) != 2 || selection.RequiredPending == nil || *selection.RequiredPending != 1 ||
		selection.OptionalPending != nil || !selection.Truncated || selection.LimitReason != "output_byte_budget" {
		t.Fatalf("unexpected bounded selection: %#v", selection)
	}
	for _, item := range selection.Candidates {
		if item.PayloadBytes < 2 {
			t.Fatalf("selection returned an invalid item size: %#v", item)
		}
	}
}

func TestSelectBudgetDeduplicatesExactItemsAndRejectsConflictingIdentity(t *testing.T) {
	first := budgetCandidate("same", false, SliceNeighbor, `{"id":"same"}`)
	second := budgetCandidate("same", true, SliceTarget, `{"id":"same"}`)
	options := BudgetOptions{BaseBytes: 1, MaxBytes: 100, MaxItems: 10, DeliveryMultiplier: 1}
	selection, err := SelectBudget(context.Background(), []BudgetCandidate{first, second}, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) != 1 || !selection.Candidates[0].Required {
		t.Fatalf("identical item was not deduplicated with required status preserved: %#v", selection)
	}
	second.PayloadDigest = model.SHA256([]byte(`{"id":"other"}`))
	if _, err := SelectBudget(context.Background(), []BudgetCandidate{first, second}, options); !errors.Is(err, ErrInvalidBudgetInput) {
		t.Fatalf("conflicting duplicate identity accepted: %v", err)
	}
}

func TestGroupGapsBoundsMetadataAndPreservesOverflowCount(t *testing.T) {
	_, identity := testScope(t)
	gaps := make([]Gap, 0, 66)
	for i := 0; i < 65; i++ {
		gaps = append(gaps, Gap{Code: fmt.Sprintf("gap_%03d", i), Reason: "bounded safety detail"})
	}
	for i := 0; i < 16; i++ {
		gaps = append(gaps, Gap{
			Code: "many_sources", Reason: "source references exceed the wire bound",
			SourceRefs: []model.SourceRef{testSource(identity, "pkg/a.go", int64(i), int64(i+1))},
		})
	}
	gaps = append(gaps, Gap{
		Code: "many_sources", Reason: "source references exceed the wire bound",
		SourceRefs: []model.SourceRef{testSource(identity, "pkg/a.go", 16, 17)},
	})

	grouped, err := GroupGaps(context.Background(), gaps, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(grouped.Gaps) != 64 || !grouped.MetadataTruncated || grouped.DetailsOmitted != 20 {
		t.Fatalf("gap metadata was not safely bounded: %#v", grouped)
	}
	last := grouped.Gaps[len(grouped.Gaps)-1]
	if last.Code != "gap_details_pending" || last.Count == nil || *last.Count != grouped.DetailsOmitted {
		t.Fatalf("missing explicit grouped-details gap: %#v", last)
	}
}

func TestBudgetOperationsPropagateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SelectBudget(ctx, nil, BudgetOptions{
		BaseBytes: 1, MaxBytes: 100, MaxItems: 1, DeliveryMultiplier: 1,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("selection ignored cancellation: %v", err)
	}
	if _, err := GroupGaps(ctx, nil, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("gap grouping ignored cancellation: %v", err)
	}
}

func TestSelectBudgetCountsDualDeliveryBytes(t *testing.T) {
	candidate := budgetCandidate("item", true, SliceTarget, `{"id":"item"}`)
	selection, err := SelectBudget(context.Background(), []BudgetCandidate{candidate}, BudgetOptions{
		BaseBytes: 90, MaxBytes: 110, MaxItems: 10, DeliveryMultiplier: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) != 0 || selection.RequiredPending == nil || *selection.RequiredPending != 1 {
		t.Fatalf("dual delivery admitted an item beyond the delivered-byte cap: %#v", selection)
	}
}

func TestSelectBudgetDoesNotUnderflowWhenEnvelopeExceedsMaximum(t *testing.T) {
	candidate := budgetCandidate("required", true, SliceTarget, `{"id":"required"}`)
	selection, err := SelectBudget(context.Background(), []BudgetCandidate{candidate}, BudgetOptions{
		BaseBytes: model.MaxSafeInteger, MaxBytes: 4096, MaxItems: 10, DeliveryMultiplier: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) != 0 || selection.RequiredPending == nil || *selection.RequiredPending != 1 {
		t.Fatalf("item was admitted despite an over-budget response envelope: %#v", selection)
	}
}

func TestSelectBudgetRejectsUnknownPriorityCategory(t *testing.T) {
	candidate := budgetCandidate("item", false, SliceCategory("unknown"), `{"id":"item"}`)
	_, err := SelectBudget(context.Background(), []BudgetCandidate{candidate}, BudgetOptions{
		BaseBytes: 1, MaxBytes: 4096, MaxItems: 10, DeliveryMultiplier: 1,
	})
	if !errors.Is(err, ErrInvalidBudgetInput) {
		t.Fatalf("unknown priority category was accepted: %v", err)
	}
}

func budgetCandidate(id string, required bool, category SliceCategory, payload string) BudgetCandidate {
	raw := []byte(payload)
	return BudgetCandidate{ID: id, Required: required, Category: category, PayloadBytes: int64(len(raw)), PayloadDigest: model.SHA256(raw)}
}
