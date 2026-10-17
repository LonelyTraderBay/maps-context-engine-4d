package response

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/contextpack"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestBuildPackProducesValidFixedPointResponseWithinDualDeliveryCap(t *testing.T) {
	builder := testBuilder(t)
	input := testPackInput()
	input.Items = []ItemCandidate{{Item: testEvidenceItem("target", true, "complete target evidence"), Category: contextpack.SliceTarget}}

	built, err := builder.BuildPack(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(built.JSON) || int64(len(built.JSON))*2 > input.MaxOutputBytes {
		t.Fatalf("dual-delivery response exceeded cap or is invalid JSON: len=%d cap=%d", len(built.JSON), input.MaxOutputBytes)
	}
	if built.Response.Budget.SerializedBytes != int64(len(built.JSON))*2 || built.Response.Budget.DeliveryMode != "mcp_dual" {
		t.Fatalf("serialized byte fixed point is wrong: %#v, JSON=%d", built.Response.Budget, len(built.JSON))
	}
	if len(built.Response.Data.Items) != 1 || !built.Response.Data.Items[0].RequiredForReview || built.Response.Readiness != "reviewable" {
		t.Fatalf("complete mandatory evidence was not preserved: %#v", built.Response)
	}
}

func TestBuildPackReportsRequiredEvidenceWhenWholeItemExceedsBudget(t *testing.T) {
	builder := testBuilder(t)
	input := testPackInput()
	input.Items = []ItemCandidate{{
		Item: testEvidenceItem("large-required", true, strings.Repeat("x", 2500)), Category: contextpack.SliceSource,
	}}

	built, err := builder.BuildPack(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Response.Data.Items) != 0 || built.Response.Readiness != "needs_context" ||
		built.Response.Pagination.RequiredPending == nil || *built.Response.Pagination.RequiredPending != 1 ||
		!built.Response.Pagination.Truncated {
		t.Fatalf("required evidence omission was hidden: %#v", built.Response)
	}
	if int64(len(built.JSON))*2 > input.MaxOutputBytes || built.Response.Budget.SerializedBytes != int64(len(built.JSON))*2 {
		t.Fatalf("partial response exceeded cap or misreported delivered bytes: %d %#v", len(built.JSON), built.Response.Budget)
	}
}

func TestBuildPackReturnsTypedBudgetTooSmallWhenSafetyEnvelopeCannotFit(t *testing.T) {
	builder := testBuilder(t)
	input := testPackInput()
	input.TaskSlice.Gaps = make([]contextpack.Gap, 64)
	for index := range input.TaskSlice.Gaps {
		input.TaskSlice.Gaps[index] = contextpack.Gap{
			Code:   "gap_" + strconv.Itoa(index),
			Reason: strings.Repeat("safety detail ", 70),
		}
	}

	built, err := builder.BuildPack(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if built.Response.Status != "error" || built.Response.Error == nil || built.Response.Error.Code != "BUDGET_TOO_SMALL" || built.Response.Data != nil {
		t.Fatalf("unfit mandatory envelope did not return typed budget error: %#v", built.Response)
	}
	if int64(len(built.JSON))*2 > input.MaxOutputBytes || !json.Valid(built.JSON) {
		t.Fatalf("budget error response exceeded cap or was invalid JSON: len=%d", len(built.JSON))
	}
}

func TestBuildPackRequiresVerifiedHostForStructuredOnlyDelivery(t *testing.T) {
	builder := testBuilder(t)
	input := testPackInput()
	input.DeliveryMode = "mcp_structured_verified"
	if _, err := builder.BuildPack(context.Background(), input); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("unverified structured host mode was accepted: %v", err)
	}
	input.StructuredOutputHostVerified = true
	built, err := builder.BuildPack(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if built.Response.Budget.DeliveryMode != "mcp_structured_verified" || built.Response.Budget.SerializedBytes != int64(len(built.JSON)) {
		t.Fatalf("verified structured host delivery has wrong accounting: %#v len=%d", built.Response.Budget, len(built.JSON))
	}
}

func TestBuildPackBoundsGroupedGapMetadata(t *testing.T) {
	builder := testBuilder(t)
	input := testPackInput()
	input.DeliveryMode = "cli"
	input.MaxOutputBytes = 131072
	input.TaskSlice.Gaps = make([]contextpack.Gap, 1000)
	for index := range input.TaskSlice.Gaps {
		input.TaskSlice.Gaps[index] = contextpack.Gap{Code: "gap_" + strconv.Itoa(index), Reason: "distinct safety gap"}
	}

	built, err := builder.BuildPack(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Response.Coverage.Gaps) > 64 || len(built.Response.Warnings) > 64 ||
		built.Response.Readiness != "needs_context" || !built.Response.Pagination.Truncated {
		t.Fatalf("gap metadata was not grouped with an explicit pending signal: %#v", built.Response)
	}
}

func testBuilder(t *testing.T) *Builder {
	t.Helper()
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	builder, err := NewBuilder(validator)
	if err != nil {
		t.Fatal(err)
	}
	return builder
}

func testPackInput() PackInput {
	zero := 0
	return PackInput{
		RequestID: model.UUID("00000000-0000-4000-8000-000000000001"),
		Freshness: Freshness{
			Status: "not_checked", Scope: "none", CheckedAt: nil, BufferScope: "disk_only",
			Consistency: "captured_bytes_with_observed_stability", ChangedCount: nil, SourceView: "indexed_snapshot",
		},
		PackToken: "opaque-test-token", TargetRefs: []json.RawMessage{},
		TaskSlice: contextpack.TaskSlice{
			Items: []contextpack.TaskSliceItem{}, TrustedRules: []contextpack.TrustedRuleAnchor{},
			Constraints: []contextpack.ConstraintDisposition{}, Gaps: []contextpack.Gap{},
			Validation:          contextpack.ValidationPlan{Status: "reviewable", Steps: []contextpack.ValidationStep{}},
			EnumerationComplete: true, RequiredPending: &zero,
		},
		Capabilities: []Capability{}, ScopeDescription: "one captured scope",
		MaxOutputBytes: 4096, DeliveryMode: "mcp_dual",
	}
}

func testEvidenceItem(id string, required bool, content string) EvidenceItem {
	return EvidenceItem{
		ItemID: id, Kind: "overview", RequiredForReview: required, WhyIncluded: "selected_target",
		SourceRefs: []SourceRef{}, Content: &content, EvidenceRefs: []EvidenceRef{}, Limitations: []Gap{}, FragmentOf: nil,
	}
}
