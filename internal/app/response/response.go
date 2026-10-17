// Package response builds schema-validated, byte-bounded application responses.
package response

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/contextpack"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	DefaultMaxOutputBytes int64 = 24576
	MinMaxOutputBytes     int64 = 4096
	MaxMaxOutputBytes     int64 = 131072
	maxFixedPointPasses         = 8
)

var (
	ErrInvalidResponse = errors.New("invalid response input")
	ErrBudgetTooSmall  = errors.New("response envelope exceeds output budget")
	ErrSerialization   = errors.New("response serialization failed")
)

type ResponseValidator interface {
	ValidateResponse(operation string, raw []byte) error
}

type Builder struct {
	validator ResponseValidator
}

func NewBuilder(validator ResponseValidator) (*Builder, error) {
	if validator == nil {
		return nil, fmt.Errorf("%w: response validator is required", ErrInvalidResponse)
	}
	return &Builder{validator: validator}, nil
}

type Freshness struct {
	Status       string  `json:"status"`
	Scope        string  `json:"scope"`
	CheckedAt    *string `json:"checked_at"`
	BufferScope  string  `json:"buffer_scope"`
	Consistency  string  `json:"consistency"`
	ChangedCount *int64  `json:"changed_count"`
	SourceView   string  `json:"source_view"`
}

type Capability struct {
	Name            string `json:"name"`
	ProviderID      string `json:"provider_id"`
	ProviderVersion string `json:"provider_version"`
	Status          string `json:"status"`
}

type SourceRef struct {
	WorktreeID   model.UUID      `json:"worktree_id"`
	ProfileID    model.Hash      `json:"profile_id"`
	GenerationID model.UUID      `json:"generation_id"`
	Path         string          `json:"path"`
	ContentHash  model.Hash      `json:"content_hash"`
	Range        model.ByteRange `json:"range"`
}

type Gap struct {
	Code       string      `json:"code"`
	Reason     string      `json:"reason"`
	Count      *int64      `json:"count"`
	SourceRefs []SourceRef `json:"source_refs"`
}

type EvidenceRef struct {
	EvidenceID string      `json:"evidence_id"`
	Origin     string      `json:"origin"`
	Kind       string      `json:"kind"`
	SourceRefs []SourceRef `json:"source_refs"`
	RecordRef  *RecordRef  `json:"record_ref"`
	ArtifactID *model.Hash `json:"artifact_id"`
}

type RecordRef struct {
	RecordID model.UUID `json:"record_id"`
	Version  int64      `json:"version"`
}

type EvidenceItem struct {
	ItemID            string        `json:"item_id"`
	Kind              string        `json:"kind"`
	RequiredForReview bool          `json:"required_for_review"`
	WhyIncluded       string        `json:"why_included"`
	SourceRefs        []SourceRef   `json:"source_refs"`
	Content           *string       `json:"content"`
	EvidenceRefs      []EvidenceRef `json:"evidence_refs"`
	Limitations       []Gap         `json:"limitations"`
	FragmentOf        *SourceRef    `json:"fragment_of"`
}

type PackData struct {
	Operation                 string            `json:"operation"`
	PackToken                 string            `json:"pack_token"`
	TargetRefs                []json.RawMessage `json:"target_refs"`
	Items                     []EvidenceItem    `json:"items"`
	EditPreconditionValidated bool              `json:"edit_precondition_validated"`
}

type Coverage struct {
	Status           string       `json:"status"`
	Capabilities     []Capability `json:"capabilities"`
	Gaps             []Gap        `json:"gaps"`
	ScopeDescription string       `json:"scope_description"`
}

type Pagination struct {
	Truncated           bool    `json:"truncated"`
	EnumerationComplete bool    `json:"enumeration_complete"`
	ReturnedCount       int64   `json:"returned_count"`
	RequiredPending     *int64  `json:"required_pending"`
	OptionalPending     *int64  `json:"optional_pending"`
	NextCursor          *string `json:"next_cursor"`
	LimitReason         *string `json:"limit_reason"`
}

type Budget struct {
	MaxOutputBytes  int64   `json:"max_output_bytes"`
	SerializedBytes int64   `json:"serialized_bytes"`
	TokenCount      *int64  `json:"token_count"`
	TokenEstimate   *int64  `json:"token_estimate"`
	TokenizerID     *string `json:"tokenizer_id"`
	EstimateOnly    bool    `json:"estimate_only"`
	DeliveryMode    string  `json:"delivery_mode"`
}

type Error struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	Retryable       bool   `json:"retryable"`
	SuggestedAction string `json:"suggested_action"`
	Details         []Gap  `json:"details"`
}

type Response struct {
	SchemaVersion int64              `json:"schema_version"`
	RequestID     model.UUID         `json:"request_id"`
	Status        string             `json:"status"`
	Scope         *model.ScopeVector `json:"scope"`
	Freshness     Freshness          `json:"freshness"`
	Coverage      Coverage           `json:"coverage"`
	Readiness     string             `json:"readiness"`
	Data          *PackData          `json:"data"`
	Pagination    Pagination         `json:"pagination"`
	Budget        Budget             `json:"budget"`
	Warnings      []Gap              `json:"warnings"`
	Error         *Error             `json:"error"`
}

type ItemCandidate struct {
	Item     EvidenceItem
	Category contextpack.SliceCategory
}

type PackInput struct {
	RequestID                 model.UUID
	Scope                     *model.ScopeVector
	Freshness                 Freshness
	PackToken                 string
	TargetRefs                []json.RawMessage
	Items                     []ItemCandidate
	TaskSlice                 contextpack.TaskSlice
	Capabilities              []Capability
	ScopeDescription          string
	EditPreconditionValidated bool
	MaxOutputBytes            int64
	DeliveryMode              string
	// StructuredOutputHostVerified is set only after the selected MCP host has
	// passed its structured-content compatibility fixture.
	StructuredOutputHostVerified bool
}

type Built struct {
	Response Response
	JSON     []byte
}

// BuildPack admits complete evidence items, then serializes and validates the exact S43 response.
func (builder *Builder) BuildPack(ctx context.Context, input PackInput) (Built, error) {
	if builder == nil || builder.validator == nil || ctx == nil {
		return Built{}, fmt.Errorf("%w: builder and context are required", ErrInvalidResponse)
	}
	if err := ctx.Err(); err != nil {
		return Built{}, err
	}
	if err := validatePackInput(input); err != nil {
		return Built{}, err
	}
	maxBytes := input.MaxOutputBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxOutputBytes
	}
	mode := input.DeliveryMode
	if mode == "" {
		mode = "mcp_dual"
	}
	multiplier, err := deliveryMultiplier(mode, input.StructuredOutputHostVerified)
	if err != nil {
		return Built{}, err
	}

	grouped, err := contextpack.GroupGaps(ctx, input.TaskSlice.Gaps, 64)
	if err != nil {
		return Built{}, err
	}
	baseResponse := responseForInput(input, maxBytes, mode, grouped.Gaps)
	baseResponse.Data.Items = make([]EvidenceItem, 0)
	setConservativeBudgetFields(&baseResponse)
	base, err := json.Marshal(baseResponse)
	if err != nil {
		return Built{}, fmt.Errorf("%w: encode base response: %v", ErrSerialization, err)
	}
	if err := builder.validator.ValidateResponse("mce4d_context_pack", base); err != nil {
		return Built{}, fmt.Errorf("%w: base response does not match S43: %v", ErrInvalidResponse, err)
	}
	baseDelivered, ok := multiplyBytes(int64(len(base)), multiplier)
	if !ok {
		return Built{}, fmt.Errorf("%w: base response byte count overflow", ErrSerialization)
	}

	candidates := make([]contextpack.BudgetCandidate, 0, len(input.Items))
	for _, candidate := range input.Items {
		if err := ctx.Err(); err != nil {
			return Built{}, err
		}
		if candidate.Item.ItemID == "" {
			return Built{}, fmt.Errorf("%w: item ID is required", ErrInvalidResponse)
		}
		item := cloneEvidenceItem(candidate.Item)
		if err := validateEvidenceItem(item); err != nil {
			return Built{}, err
		}
		if !validSliceCategory(candidate.Category) {
			return Built{}, fmt.Errorf("%w: unsupported evidence priority category", ErrInvalidResponse)
		}
		payload, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return Built{}, fmt.Errorf("%w: encode evidence item: %v", ErrSerialization, marshalErr)
		}
		candidates = append(candidates, contextpack.BudgetCandidate{
			ID: candidate.Item.ItemID, Required: candidate.Item.RequiredForReview,
			Category: candidate.Category, PayloadBytes: int64(len(payload)), PayloadDigest: model.SHA256(payload),
		})
	}
	selection, err := contextpack.SelectBudget(ctx, candidates, contextpack.BudgetOptions{
		BaseBytes: baseDelivered, MaxBytes: maxBytes, MaxItems: contextpack.MaxResponseItems,
		DeliveryMultiplier: multiplier,
	})
	if err != nil {
		return Built{}, err
	}
	response := responseForInput(input, maxBytes, mode, grouped.Gaps)
	response.Data.Items = make([]EvidenceItem, 0, len(selection.Candidates))
	selectedIDs := make(map[string]struct{}, len(selection.Candidates))
	for _, candidate := range selection.Candidates {
		selectedIDs[candidate.ID] = struct{}{}
	}
	selectedItems := make(map[string]EvidenceItem, len(selectedIDs))
	for _, candidate := range input.Items {
		if _, selected := selectedIDs[candidate.Item.ItemID]; selected {
			selectedItems[candidate.Item.ItemID] = cloneEvidenceItem(candidate.Item)
		}
	}
	for _, candidate := range selection.Candidates {
		item, ok := selectedItems[candidate.ID]
		if !ok {
			return Built{}, fmt.Errorf("%w: selected item disappeared during response construction", ErrInvalidResponse)
		}
		response.Data.Items = append(response.Data.Items, item)
	}
	applyPagination(&response, input.TaskSlice, selection, grouped)

	serialized, err := marshalFixedPoint(&response, multiplier, maxBytes)
	if errors.Is(err, ErrBudgetTooSmall) {
		response = budgetTooSmallResponse(input, maxBytes, mode)
		serialized, err = marshalFixedPoint(&response, multiplier, maxBytes)
	}
	if err != nil {
		return Built{}, err
	}
	if err := builder.validator.ValidateResponse("mce4d_context_pack", serialized); err != nil {
		return Built{}, fmt.Errorf("%w: schema validation: %v", ErrSerialization, err)
	}
	return Built{Response: response, JSON: serialized}, nil
}

func validatePackInput(input PackInput) error {
	if _, err := model.ParseUUID(string(input.RequestID)); err != nil {
		return fmt.Errorf("%w: invalid request ID", ErrInvalidResponse)
	}
	if input.PackToken == "" || len(input.PackToken) > 32768 || !utf8.ValidString(input.PackToken) {
		return fmt.Errorf("%w: pack token is empty or invalid", ErrInvalidResponse)
	}
	maxBytes := input.MaxOutputBytes
	if maxBytes != 0 && (maxBytes < MinMaxOutputBytes || maxBytes > MaxMaxOutputBytes) {
		return fmt.Errorf("%w: max output bytes must be within %d..%d", ErrInvalidResponse, MinMaxOutputBytes, MaxMaxOutputBytes)
	}
	if len(input.TargetRefs) > 32 || len(input.Capabilities) > 128 || len(input.Items) > 500001 {
		return fmt.Errorf("%w: pack metadata exceeds request bounds", ErrInvalidResponse)
	}
	if !utf8.ValidString(input.ScopeDescription) || len(input.ScopeDescription) == 0 || utf8.RuneCountInString(input.ScopeDescription) > 1024 {
		return fmt.Errorf("%w: scope description must be valid UTF-8 within 1..1024 characters", ErrInvalidResponse)
	}
	if input.Scope != nil && (len(input.Scope.Members) > 32 || len(input.Scope.UnavailableMembers) > 32) {
		return fmt.Errorf("%w: scope membership exceeds the frozen schema limit", ErrInvalidResponse)
	}
	if input.TaskSlice.RequiredPending != nil && (*input.TaskSlice.RequiredPending < 0 || int64(*input.TaskSlice.RequiredPending) > model.MaxSafeInteger) {
		return fmt.Errorf("%w: required pending count is outside the safe integer range", ErrInvalidResponse)
	}
	var targetBytes int64
	for _, target := range input.TargetRefs {
		if !json.Valid(target) || len(bytes.TrimSpace(target)) == 0 {
			return fmt.Errorf("%w: target reference is not valid JSON", ErrInvalidResponse)
		}
		if int64(len(target)) > MaxMaxOutputBytes-targetBytes {
			return fmt.Errorf("%w: target references exceed the bounded response input", ErrInvalidResponse)
		}
		targetBytes += int64(len(target))
	}
	return nil
}

func responseForInput(input PackInput, maxBytes int64, mode string, gaps []contextpack.Gap) Response {
	responseGaps := make([]Gap, 0, len(gaps))
	for _, gap := range gaps {
		responseGaps = append(responseGaps, toWireGap(gap))
	}
	coverageStatus := "supported_scope_complete"
	if !input.TaskSlice.EnumerationComplete || len(gaps) > 0 {
		coverageStatus = "partial"
	}
	response := Response{
		SchemaVersion: 1,
		RequestID:     input.RequestID,
		Status:        "ok",
		Scope:         cloneScope(input.Scope),
		Freshness:     cloneFreshness(input.Freshness),
		Coverage: Coverage{
			Status: coverageStatus, Capabilities: cloneCapabilities(input.Capabilities),
			Gaps: responseGaps, ScopeDescription: input.ScopeDescription,
		},
		Readiness: "reviewable",
		Data: &PackData{
			Operation: "context_pack", PackToken: input.PackToken,
			TargetRefs: cloneRawMessages(input.TargetRefs), Items: make([]EvidenceItem, 0),
			EditPreconditionValidated: input.EditPreconditionValidated,
		},
		Pagination: Pagination{
			Truncated: false, EnumerationComplete: input.TaskSlice.EnumerationComplete,
			ReturnedCount: 0, RequiredPending: cloneInt(input.TaskSlice.RequiredPending),
			OptionalPending: nil, NextCursor: nil, LimitReason: nil,
		},
		Budget: Budget{
			MaxOutputBytes: maxBytes, SerializedBytes: 0,
			TokenCount: nil, TokenEstimate: nil, TokenizerID: nil,
			EstimateOnly: false, DeliveryMode: mode,
		},
		Warnings: cloneGaps(responseGaps), Error: nil,
	}
	return response
}

func applyPagination(response *Response, taskSlice contextpack.TaskSlice, selection contextpack.BudgetSelection, grouped contextpack.GapSummary) {
	response.Pagination.ReturnedCount = int64(len(selection.Candidates))
	response.Pagination.RequiredPending = combinePending(taskSlice.RequiredPending, selection.RequiredPending)
	response.Pagination.OptionalPending = cloneInt(selection.OptionalPending)
	response.Pagination.Truncated = selection.Truncated || taskSlice.ContinuationRequired || grouped.MetadataTruncated
	response.Pagination.EnumerationComplete = taskSlice.EnumerationComplete
	if selection.LimitReason != "" {
		response.Pagination.LimitReason = stringPointer(selection.LimitReason)
	} else if grouped.MetadataTruncated {
		response.Pagination.LimitReason = stringPointer("gap_metadata_limit")
	} else if taskSlice.ContinuationRequired {
		response.Pagination.LimitReason = stringPointer("traversal_limit")
	}
	if response.Pagination.Truncated || len(response.Coverage.Gaps) > 0 {
		response.Status = "partial"
		response.Coverage.Status = "partial"
	}
	if response.Pagination.RequiredPending == nil || *response.Pagination.RequiredPending > 0 ||
		!taskSlice.EnumerationComplete || len(response.Coverage.Gaps) > 0 || grouped.MetadataTruncated {
		response.Readiness = "needs_context"
	}
}

func budgetTooSmallResponse(input PackInput, maxBytes int64, mode string) Response {
	gap := Gap{Code: "required_evidence_pending", Reason: "The minimum response envelope does not fit the configured byte budget.", SourceRefs: make([]SourceRef, 0)}
	return Response{
		SchemaVersion: 1, RequestID: input.RequestID, Status: "error", Scope: nil,
		Freshness: cloneFreshness(input.Freshness),
		Coverage: Coverage{
			Status: "unknown", Capabilities: make([]Capability, 0),
			Gaps: []Gap{gap}, ScopeDescription: "Context pack could not be delivered within the configured output budget.",
		},
		Readiness: "blocked", Data: nil,
		Pagination: Pagination{
			Truncated: true, EnumerationComplete: false, ReturnedCount: 0,
			RequiredPending: nil, OptionalPending: nil, NextCursor: nil,
			LimitReason: stringPointer("output_byte_budget"),
		},
		Budget: Budget{
			MaxOutputBytes: maxBytes, SerializedBytes: 0, EstimateOnly: false, DeliveryMode: mode,
		},
		Warnings: []Gap{gap},
		Error: &Error{
			Code: "BUDGET_TOO_SMALL", Message: "Required context and safety metadata exceed the configured output budget.",
			Retryable: false, SuggestedAction: "expand", Details: []Gap{gap},
		},
	}
}

func marshalFixedPoint(response *Response, multiplier int64, maxBytes int64) ([]byte, error) {
	for range maxFixedPointPasses {
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSerialization, err)
		}
		delivered, ok := multiplyBytes(int64(len(raw)), multiplier)
		if !ok || delivered > maxBytes {
			return nil, ErrBudgetTooSmall
		}
		if response.Budget.SerializedBytes == delivered {
			return raw, nil
		}
		response.Budget.SerializedBytes = delivered
	}
	return nil, fmt.Errorf("%w: serialized byte count did not converge", ErrSerialization)
}

func setConservativeBudgetFields(response *Response) {
	maxSafe := model.MaxSafeInteger
	response.Status = "partial"
	response.Readiness = "needs_context"
	response.Budget.SerializedBytes = maxSafe
	response.Pagination.ReturnedCount = maxSafe
	response.Pagination.RequiredPending = intPointer(maxSafe)
	response.Pagination.OptionalPending = intPointer(maxSafe)
	response.Pagination.Truncated = false
	response.Pagination.EnumerationComplete = false
	response.Pagination.LimitReason = stringPointer(strings.Repeat("x", 192))
}

func deliveryMultiplier(mode string, structuredOutputHostVerified bool) (int64, error) {
	switch mode {
	case "http", "cli", "mcp_text", "mcp_structured_verified":
		if mode == "mcp_structured_verified" && !structuredOutputHostVerified {
			return 0, fmt.Errorf("%w: structured MCP delivery requires a verified host", ErrInvalidResponse)
		}
		return 1, nil
	case "mcp_dual":
		return 2, nil
	default:
		return 0, fmt.Errorf("%w: unsupported delivery mode %q", ErrInvalidResponse, mode)
	}
}

func multiplyBytes(value int64, multiplier int64) (int64, bool) {
	if value < 0 || multiplier < 1 || value > math.MaxInt64/multiplier {
		return 0, false
	}
	return value * multiplier, true
}

func combinePending(base, omitted *int) *int64 {
	if omitted == nil {
		return cloneInt(base)
	}
	if base == nil || *base < 0 || *omitted < 0 || int64(*base) > model.MaxSafeInteger-int64(*omitted) {
		return nil
	}
	value := int64(*base) + int64(*omitted)
	return &value
}

func toWireGap(gap contextpack.Gap) Gap {
	result := Gap{Code: gap.Code, Reason: gap.Reason, SourceRefs: make([]SourceRef, 0, len(gap.SourceRefs))}
	if gap.Count != nil {
		count := int64(*gap.Count)
		result.Count = &count
	}
	for _, source := range gap.SourceRefs {
		result.SourceRefs = append(result.SourceRefs, SourceRef{
			WorktreeID: source.WorktreeID, ProfileID: source.ProfileID, GenerationID: source.GenerationID,
			Path: source.Path, ContentHash: source.ContentHash, Range: source.Range,
		})
	}
	return result
}

func cloneScope(scope *model.ScopeVector) *model.ScopeVector {
	if scope == nil {
		return nil
	}
	copyOf := *scope
	copyOf.GroupID = cloneUUID(scope.GroupID)
	copyOf.GroupRevision = cloneInt64(scope.GroupRevision)
	copyOf.Members = make([]model.ScopeMember, len(scope.Members))
	for index, member := range scope.Members {
		copyOf.Members[index] = member
		copyOf.Members[index].ServiceID = cloneIdentifier(member.ServiceID)
	}
	copyOf.UnavailableMembers = make([]model.UnavailableMember, len(scope.UnavailableMembers))
	for index, member := range scope.UnavailableMembers {
		copyOf.UnavailableMembers[index] = member
		copyOf.UnavailableMembers[index].ServiceID = cloneIdentifier(member.ServiceID)
		copyOf.UnavailableMembers[index].WorktreeID = cloneUUID(member.WorktreeID)
		copyOf.UnavailableMembers[index].ProfileID = cloneHash(member.ProfileID)
	}
	return &copyOf
}

func cloneRawMessages(values []json.RawMessage) []json.RawMessage {
	copyOf := make([]json.RawMessage, len(values))
	for i := range values {
		copyOf[i] = append(json.RawMessage(nil), values[i]...)
	}
	return copyOf
}

func cloneCapabilities(values []Capability) []Capability {
	copyOf := make([]Capability, len(values))
	copy(copyOf, values)
	return copyOf
}

func cloneInt(value *int) *int64 {
	if value == nil {
		return nil
	}
	copyOf := int64(*value)
	return &copyOf
}

func intPointer(value int64) *int64 { return &value }

func stringPointer(value string) *string { return &value }

func validateEvidenceItem(item EvidenceItem) error {
	if !validWireID(item.ItemID) || !validWireID(item.WhyIncluded) ||
		!oneOf(item.Kind, "source_range", "reference_catalog", "constraint_excerpt", "boundary", "overview", "validation_obligation", "source_range_fragment") ||
		len(item.SourceRefs) > 16 || len(item.EvidenceRefs) > 16 || len(item.Limitations) > 16 {
		return fmt.Errorf("%w: evidence item exceeds text or reference limits", ErrInvalidResponse)
	}
	if item.Content != nil && (!utf8.ValidString(*item.Content) || len(*item.Content) > 4*131072 || utf8.RuneCountInString(*item.Content) > 131072) {
		return fmt.Errorf("%w: evidence content exceeds the frozen schema limit", ErrInvalidResponse)
	}
	for _, source := range item.SourceRefs {
		if err := validateWireSourceRef(source); err != nil {
			return fmt.Errorf("%w: invalid evidence source reference", ErrInvalidResponse)
		}
	}
	for _, gap := range item.Limitations {
		if err := validateWireGap(gap); err != nil {
			return fmt.Errorf("%w: invalid evidence limitation", ErrInvalidResponse)
		}
	}
	for _, evidence := range item.EvidenceRefs {
		if !validWireID(evidence.EvidenceID) || len(evidence.SourceRefs) > 16 ||
			!oneOf(evidence.Origin, "static_analyzer", "build_instrumentation", "runtime_observation", "manual_approved", "ai_proposal") ||
			!oneOf(evidence.Kind, "syntax_exact", "type_resolved", "type_possible", "explicit_document", "text_match", "unresolved", "build_anchored", "runtime_observed", "manual_declared", "proposed") {
			return fmt.Errorf("%w: invalid evidence reference", ErrInvalidResponse)
		}
		for _, source := range evidence.SourceRefs {
			if err := validateWireSourceRef(source); err != nil {
				return fmt.Errorf("%w: invalid evidence source reference", ErrInvalidResponse)
			}
		}
		if evidence.RecordRef != nil {
			if _, err := model.ParseUUID(string(evidence.RecordRef.RecordID)); err != nil ||
				evidence.RecordRef.Version < 1 || evidence.RecordRef.Version > model.MaxSafeInteger {
				return fmt.Errorf("%w: invalid evidence record reference", ErrInvalidResponse)
			}
		}
		if evidence.ArtifactID != nil {
			if _, err := model.ParseHash(string(*evidence.ArtifactID)); err != nil {
				return fmt.Errorf("%w: invalid evidence artifact reference", ErrInvalidResponse)
			}
		}
	}
	if item.FragmentOf != nil {
		if err := validateWireSourceRef(*item.FragmentOf); err != nil {
			return fmt.Errorf("%w: invalid evidence fragment reference", ErrInvalidResponse)
		}
	}
	return nil
}

func validateWireGap(gap Gap) error {
	if !validWireID(gap.Code) || !validWireText(gap.Reason, 1024) || len(gap.SourceRefs) > 16 ||
		(gap.Count != nil && (*gap.Count < 0 || *gap.Count > model.MaxSafeInteger)) {
		return ErrInvalidResponse
	}
	for _, source := range gap.SourceRefs {
		if err := validateWireSourceRef(source); err != nil {
			return err
		}
	}
	return nil
}

func validateWireSourceRef(source SourceRef) error {
	if _, err := model.ParseUUID(string(source.WorktreeID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(source.ProfileID)); err != nil {
		return err
	}
	if _, err := model.ParseUUID(string(source.GenerationID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(source.ContentHash)); err != nil {
		return err
	}
	if len(source.Path) > 4*4096 || utf8.RuneCountInString(source.Path) > 4096 || model.ValidateRelativePath(source.Path) != nil {
		return model.ErrInvalidValue
	}
	if source.Range.StartByte < 0 || source.Range.EndByte < source.Range.StartByte || source.Range.EndByte > model.MaxSafeInteger {
		return model.ErrInvalidValue
	}
	return nil
}

func validWireID(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 1 && utf8.RuneCountInString(value) <= 192
}

func validWireText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 1 && utf8.RuneCountInString(value) <= maxRunes
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func validSliceCategory(value contextpack.SliceCategory) bool {
	switch value {
	case contextpack.SliceSafetyBoundary, contextpack.SliceTarget, contextpack.SliceIntended,
		contextpack.SliceObserved, contextpack.SliceDeclaration, contextpack.SliceSource,
		contextpack.SliceReference, contextpack.SliceDataflow, contextpack.SliceDependency,
		contextpack.SliceContract, contextpack.SliceTest, contextpack.SliceDocumentation,
		contextpack.SliceModule, contextpack.SliceLexicalCandidate, contextpack.SliceNeighbor:
		return true
	default:
		return false
	}
}

func cloneEvidenceItem(item EvidenceItem) EvidenceItem {
	if item.Content != nil {
		content := *item.Content
		item.Content = &content
	}
	item.SourceRefs = append(make([]SourceRef, 0, len(item.SourceRefs)), item.SourceRefs...)
	item.EvidenceRefs = append(make([]EvidenceRef, 0, len(item.EvidenceRefs)), item.EvidenceRefs...)
	for index := range item.EvidenceRefs {
		item.EvidenceRefs[index].SourceRefs = append(make([]SourceRef, 0, len(item.EvidenceRefs[index].SourceRefs)), item.EvidenceRefs[index].SourceRefs...)
		if item.EvidenceRefs[index].RecordRef != nil {
			ref := *item.EvidenceRefs[index].RecordRef
			item.EvidenceRefs[index].RecordRef = &ref
		}
		item.EvidenceRefs[index].ArtifactID = cloneHash(item.EvidenceRefs[index].ArtifactID)
	}
	item.Limitations = cloneGaps(item.Limitations)
	if item.FragmentOf != nil {
		fragment := *item.FragmentOf
		item.FragmentOf = &fragment
	}
	return item
}

func cloneGaps(gaps []Gap) []Gap {
	copyOf := make([]Gap, len(gaps))
	for index, gap := range gaps {
		copyOf[index] = gap
		copyOf[index].Count = cloneInt64(gap.Count)
		copyOf[index].SourceRefs = append(make([]SourceRef, 0, len(gap.SourceRefs)), gap.SourceRefs...)
	}
	return copyOf
}

func cloneFreshness(value Freshness) Freshness {
	if value.CheckedAt != nil {
		copyOf := *value.CheckedAt
		value.CheckedAt = &copyOf
	}
	if value.ChangedCount != nil {
		copyOf := *value.ChangedCount
		value.ChangedCount = &copyOf
	}
	return value
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}

func cloneUUID(value *model.UUID) *model.UUID {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}

func cloneHash(value *model.Hash) *model.Hash {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}

func cloneIdentifier(value *model.Identifier) *model.Identifier {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}
