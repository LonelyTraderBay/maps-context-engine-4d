// Package rename coordinates a validated, preview-only Go rename operation.
package rename

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/response"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/validate"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/lsp"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

const (
	operation                     = "mce4d_rename_preview"
	defaultOutputBytes      int64 = 24576
	maxOutputBytes          int64 = 131072
	defaultDeliveryMode           = "mcp_text"
	defaultPageBytesPerEdit int64 = 4096
	resultTokenVersion            = "rename-result-token-v1"
	resultTokenPurpose            = "rename-result"
	warningRuntimeLookup          = "RUNTIME_STRING_LOOKUP_REVIEW_REQUIRED"
	warningSerialization          = "IMPLICIT_SERIALIZATION_NAME_CHANGE"
)

var (
	ErrInvalidRequest   = errors.New("rename application request is invalid")
	ErrPreconditions    = errors.New("rename workspace preconditions are not current")
	ErrIdentityChanged  = errors.New("rename scope or identity changed")
	ErrOutputBudget     = errors.New("rename response exceeds its output budget")
	ErrProviderResponse = errors.New("rename provider returned an unsafe response")
)

type Selector struct {
	Kind        string     `json:"kind"`
	Path        string     `json:"path"`
	ContentHash model.Hash `json:"content_hash"`
	ByteOffset  int64      `json:"byte_offset"`
}

type ScopeRequest struct {
	GenerationID *model.UUID        `json:"generation_id,omitempty"`
	GroupID      *model.UUID        `json:"group_id,omitempty"`
	PinnedVector *model.ScopeVector `json:"pinned_vector,omitempty"`
	ProfileID    *model.Hash        `json:"profile_id,omitempty"`
	WorktreeID   *model.UUID        `json:"worktree_id,omitempty"`
}

type InitialRequest struct {
	Selector          Selector      `json:"selector"`
	NewName           string        `json:"new_name"`
	Scope             *ScopeRequest `json:"scope,omitempty"`
	PreserveContracts *bool         `json:"preserve_contracts,omitempty"`
	MaxOutputBytes    *int64        `json:"max_output_bytes,omitempty"`
}

type ContinueRequest struct {
	ResultToken    string `json:"result_token"`
	Cursor         string `json:"cursor"`
	MaxOutputBytes *int64 `json:"max_output_bytes,omitempty"`
}

// Resolver obtains server-authorized identities and immutable captures. Its
// source bytes are ephemeral and are cleared by Service after validation.
type Resolver interface {
	ResolveInitial(context.Context, Selector, *ScopeRequest) (ResolvedInitial, error)
	CurrentIdentity(context.Context) (lsp.ResultIdentity, error)
}

type ResolvedInitial struct {
	Rename        lsp.RenameRequest
	Validation    validate.Request
	SelectedBytes []byte
}

type PreconditionValidator interface {
	Validate(context.Context, validate.Request) (snapshot.ValidationResult, error)
}

type Engine interface {
	Preview(context.Context, lsp.RenameRequest) (lsp.RenamePreview, error)
	Continue(context.Context, lsp.ResultIdentity, lsp.ResultRef, string, int) (lsp.RenamePreview, error)
	ContinueResized(context.Context, lsp.ResultIdentity, lsp.ResultRef, string, int, int) (lsp.RenamePreview, error)
	Release(context.Context, lsp.ResultRef) error
}

type UUIDSource interface {
	NewUUID() (model.UUID, error)
}

type RandomUUIDSource struct{}

func (RandomUUIDSource) NewUUID() (model.UUID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read request ID entropy: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return model.ParseUUID(encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:])
}

type Service struct {
	resolver         Resolver
	preconditions    PreconditionValidator
	engine           Engine
	ids              UUIDSource
	validator        *contracts.Validator
	cursors          *cursor.CursorCodec
	pageBytesPerEdit int64
	deliveryMode     string
}

func NewService(resolver Resolver, preconditions PreconditionValidator, engine Engine, ids UUIDSource, cursors *cursor.CursorCodec) (*Service, error) {
	if resolver == nil || preconditions == nil || engine == nil || ids == nil || cursors == nil {
		return nil, fmt.Errorf("resolver, precondition validator, rename engine, ID source, and cursor codec are required: %w", ErrInvalidRequest)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("create rename contract validator: %w", err)
	}
	return &Service{
		resolver: resolver, preconditions: preconditions, engine: engine, ids: ids,
		validator: validator, cursors: cursors, pageBytesPerEdit: defaultPageBytesPerEdit, deliveryMode: defaultDeliveryMode,
	}, nil
}

// Handle accepts exactly one S43 request variant and returns a schema-checked
// response. It never applies edits to the source workspace.
func (service *Service) Handle(ctx context.Context, raw []byte) ([]byte, error) {
	if service == nil || ctx == nil || len(raw) == 0 {
		return nil, ErrInvalidRequest
	}
	if err := service.validator.ValidateRequest(operation, raw); err != nil {
		return nil, fmt.Errorf("validate S43 rename request: %w", err)
	}
	decoded, err := contracts.DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode S43 rename request: %w", ErrInvalidRequest)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, ErrInvalidRequest
	}
	if _, initial := object["selector"]; initial {
		var request InitialRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode initial rename request: %w", ErrInvalidRequest)
		}
		return service.initial(ctx, request)
	}
	var request ContinueRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode rename continuation: %w", ErrInvalidRequest)
	}
	return service.continueResult(ctx, request)
}

func (service *Service) initial(ctx context.Context, request InitialRequest) ([]byte, error) {
	maxBytes, err := requestedOutputBudget(request.MaxOutputBytes)
	if err != nil || request.Selector.Kind != "source_position" || model.ValidateRelativePath(request.Selector.Path) != nil || request.NewName == "" || len(request.NewName) > 256 {
		return nil, ErrInvalidRequest
	}
	if _, err := model.ParseHash(string(request.Selector.ContentHash)); err != nil {
		return nil, ErrInvalidRequest
	}
	resolved, err := service.resolver.ResolveInitial(ctx, request.Selector, request.Scope)
	if err != nil {
		return nil, fmt.Errorf("resolve authorized rename source: %w", err)
	}
	defer clear(resolved.SelectedBytes)
	current, err := service.resolver.CurrentIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve current rename identity: %w", err)
	}
	if !sameIdentity(current, resolved.Rename.Identity) {
		return nil, ErrIdentityChanged
	}
	if err := validateResolvedInitial(resolved, request.Selector, current); err != nil {
		return nil, err
	}
	if err := requireCurrent(service.preconditions, ctx, resolved.Validation); err != nil {
		return nil, err
	}
	preserveContracts := request.PreserveContracts == nil || *request.PreserveContracts
	resolved.Rename.NewName = request.NewName
	// The engine computes and stores the whole edit set before returning any page.
	resolved.Rename.PageSize = 1
	preview, err := service.engine.Preview(ctx, resolved.Rename)
	if err != nil {
		return nil, err
	}
	if err := validateProviderPreview(preview, true); err != nil {
		return nil, errors.Join(err, service.engine.Release(context.Background(), preview.ResultRef))
	}
	post, err := service.preconditions.Validate(ctx, resolved.Validation)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("post-rename full-workspace validation: %w", err), service.engine.Release(context.Background(), preview.ResultRef))
	}
	if !currentFreshness(post) {
		return nil, errors.Join(ErrPreconditions, service.engine.Release(context.Background(), preview.ResultRef))
	}
	postIdentity, err := service.resolver.CurrentIdentity(ctx)
	if err != nil || !sameIdentity(current, postIdentity) {
		return nil, errors.Join(ErrIdentityChanged, service.engine.Release(context.Background(), preview.ResultRef))
	}
	if err := validateEditsAgainstManifest(preview.Edits, resolved.Validation.Expected, current.Scope.Members[0]); err != nil {
		return nil, errors.Join(err, service.engine.Release(context.Background(), preview.ResultRef))
	}
	warnings, err := normalizeWarnings(preview.Prepared.Warnings)
	if err != nil {
		return nil, errors.Join(err, service.engine.Release(context.Background(), preview.ResultRef))
	}
	if len(warnings) == 0 {
		warnings, err = normalizeWarnings(preview.Warnings)
		if err != nil {
			return nil, errors.Join(err, service.engine.Release(context.Background(), preview.ResultRef))
		}
	}
	requestID, err := service.ids.NewUUID()
	if err != nil {
		return nil, errors.Join(err, service.engine.Release(context.Background(), preview.ResultRef))
	}
	pageSize := service.pageSize(maxBytes)
	for pageSize >= 1 {
		page, pageErr := service.engine.Continue(ctx, current, preview.ResultRef, "", pageSize)
		if pageErr != nil {
			return nil, errors.Join(pageErr, service.engine.Release(context.Background(), preview.ResultRef))
		}
		if !sameResultRef(page.ResultRef, preview.ResultRef) {
			return nil, errors.Join(ErrProviderResponse, service.engine.Release(context.Background(), preview.ResultRef))
		}
		if err := validateProviderPreview(page, false); err != nil {
			return nil, errors.Join(err, service.engine.Release(context.Background(), preview.ResultRef))
		}
		mask := warningMask(warnings)
		resultToken, tokenErr := service.issueResultToken(current, page.ResultRef, pageSize, maxBytes, mask, preserveContracts)
		if tokenErr != nil {
			return nil, errors.Join(tokenErr, service.engine.Release(context.Background(), preview.ResultRef))
		}
		encoded, buildErr := service.buildResponse(ctx, requestID, current, post, page, resultToken, maxBytes, preserveContracts, warnings)
		if buildErr == nil {
			return encoded, nil
		}
		if !errors.Is(buildErr, ErrOutputBudget) {
			return nil, errors.Join(buildErr, service.engine.Release(context.Background(), preview.ResultRef))
		}
		if pageSize == 1 {
			if err := service.engine.Release(context.Background(), preview.ResultRef); err != nil {
				return nil, errors.Join(buildErr, err)
			}
			return service.outputBudgetResponse(ctx, requestID, maxBytes)
		}
		pageSize /= 2
	}
	return nil, errors.Join(ErrOutputBudget, service.engine.Release(context.Background(), preview.ResultRef))
}

func (service *Service) continueResult(ctx context.Context, request ContinueRequest) ([]byte, error) {
	identity, err := service.resolver.CurrentIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve current rename identity: %w", err)
	}
	ref, pageSize, tokenMax, mask, preserveContracts, err := service.verifyResultToken(request.ResultToken, identity)
	if err != nil {
		return nil, err
	}
	tokenPageSize := pageSize
	requestedMax, err := requestedOutputBudget(request.MaxOutputBytes)
	if err != nil {
		return nil, err
	}
	if request.MaxOutputBytes == nil {
		requestedMax = tokenMax
	}
	maxBytes := requestedMax
	page, err := service.engine.Continue(ctx, identity, ref, request.Cursor, pageSize)
	if err != nil {
		return nil, err
	}
	if !sameResultRef(page.ResultRef, ref) {
		return nil, ErrProviderResponse
	}
	if err := validateProviderPreview(page, false); err != nil {
		return nil, err
	}
	requestID, err := service.ids.NewUUID()
	if err != nil {
		return nil, err
	}
	unknown := snapshot.UnknownValidation(time.Time{}, "FRESHNESS_NOT_RECHECKED_ON_CONTINUATION")
	unknown.Freshness.CheckedAt = nil
	warnings := warningsFromMask(mask)
	for {
		resultToken := request.ResultToken
		if pageSize != tokenPageSize || maxBytes != tokenMax {
			resultToken, err = service.issueResultToken(identity, ref, pageSize, maxBytes, mask, preserveContracts)
			if err != nil {
				return nil, err
			}
		}
		encoded, buildErr := service.buildResponse(ctx, requestID, identity, unknown, page, resultToken, maxBytes, preserveContracts, warnings)
		if buildErr == nil {
			return encoded, nil
		}
		if !errors.Is(buildErr, ErrOutputBudget) {
			return nil, buildErr
		}
		if pageSize == 1 {
			return service.outputBudgetResponse(ctx, requestID, maxBytes)
		}
		nextPageSize := pageSize / 2
		// The incoming cursor is always authenticated with the original token
		// page size, even when this loop has already reduced the response page.
		page, err = service.engine.ContinueResized(ctx, identity, ref, request.Cursor, tokenPageSize, nextPageSize)
		if err != nil {
			return nil, err
		}
		if !sameResultRef(page.ResultRef, ref) {
			return nil, ErrProviderResponse
		}
		if err := validateProviderPreview(page, false); err != nil {
			return nil, err
		}
		pageSize = nextPageSize
	}
}

func (service *Service) buildResponse(ctx context.Context, requestID model.UUID, identity lsp.ResultIdentity, freshness snapshot.ValidationResult, page lsp.RenamePreview, resultToken string, maxBytes int64, preserveContracts bool, warningCodes []string) ([]byte, error) {
	if len(identity.Scope.Members) != 1 {
		return nil, ErrIdentityChanged
	}
	member := identity.Scope.Members[0]
	edits := make([]ProposedEdit, 0, len(page.Edits))
	for _, raw := range page.Edits {
		var edit lsp.RenameEdit
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&edit); err != nil {
			return nil, fmt.Errorf("decode immutable rename edit: %w", ErrProviderResponse)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, ErrProviderResponse
		}
		if edit.Path == "" || utf8.RuneCountInString(edit.Path) > 4096 || model.ValidateRelativePath(edit.Path) != nil || !validHash(edit.ContentHash) || edit.Version < 1 || edit.NewText == "" || utf8.RuneCountInString(edit.NewText) > 131072 || edit.Range.StartByte < 0 || edit.Range.EndByte < edit.Range.StartByte {
			return nil, ErrProviderResponse
		}
		edits = append(edits, ProposedEdit{SourceRef: response.SourceRef{
			WorktreeID: member.WorktreeID, ProfileID: member.ProfileID, GenerationID: member.GenerationID,
			Path: edit.Path, ContentHash: edit.ContentHash, Range: edit.Range,
		}, NewText: edit.NewText})
	}
	warnings := make([]response.Gap, 0, len(warningCodes))
	conflicts := make([]response.Gap, 0)
	for _, code := range warningCodes {
		gap := warningGap(code)
		warnings = append(warnings, gap)
		if preserveContracts && code == warningSerialization {
			conflicts = append(conflicts, gap)
		}
	}
	status, readiness := "ok", "reviewable"
	if page.HasMore || len(conflicts) > 0 || !currentFreshness(freshness) {
		status, readiness = "partial", "needs_context"
	}
	var nextCursor *string
	var limitReason *string
	if page.HasMore {
		cursor := page.NextCursor
		nextCursor = &cursor
		reason := "PAGE_SIZE"
		limitReason = &reason
	}
	pending := page.ResultRef.EditCount - page.Position
	if pending < 0 {
		return nil, ErrProviderResponse
	}
	responseValue := RenameResponse{
		SchemaVersion: 1, RequestID: requestID, Status: status, Scope: cloneScope(identity.Scope),
		Freshness: responseFreshness(freshness.Freshness),
		Coverage: response.Coverage{
			Status:       "supported_scope_complete",
			Capabilities: []response.Capability{{Name: "go_rename_preview", ProviderID: "gopls", ProviderVersion: "pinned", Status: "supported_scope_complete"}},
			Gaps:         []response.Gap{}, ScopeDescription: "Go identifiers in the selected captured workspace and pinned module roots",
		},
		Readiness: readiness,
		Data: &RenameData{
			Operation: "rename_preview", Applied: false, SourceMutationCount: 0, ValidationRequired: true,
			Edits: edits, Conflicts: conflicts, PreviewComplete: !page.HasMore, ResultToken: resultToken,
			ResultID: page.ResultRef.ResultID, ResultDigest: page.ResultRef.Digest,
			TotalEdits: page.ResultRef.EditCount, ComputationComplete: true,
		},
		Pagination: response.Pagination{
			Truncated: page.HasMore, EnumerationComplete: true, ReturnedCount: int64(len(edits)),
			RequiredPending: &pending, OptionalPending: nil, NextCursor: nextCursor, LimitReason: limitReason,
		},
		Budget:   response.Budget{MaxOutputBytes: maxBytes, TokenCount: nil, TokenEstimate: nil, TokenizerID: nil, EstimateOnly: false, DeliveryMode: service.deliveryMode},
		Warnings: warnings, Error: nil,
	}
	for attempts := 0; attempts < 4; attempts++ {
		raw, err := json.Marshal(responseValue)
		if err != nil {
			return nil, fmt.Errorf("serialize rename response: %w", err)
		}
		responseValue.Budget.SerializedBytes = int64(len(raw))
		withBudget, err := json.Marshal(responseValue)
		if err != nil {
			return nil, fmt.Errorf("serialize rename response budget: %w", err)
		}
		if int64(len(withBudget)) > maxBytes || mcpDualUpperBound(withBudget) > maxBytes {
			return nil, fmt.Errorf("serialized rename response exceeds its %d byte delivery limit: %w", maxBytes, ErrOutputBudget)
		}
		if int64(len(withBudget)) == responseValue.Budget.SerializedBytes {
			if err := service.validator.ValidateResponse(operation, withBudget); err != nil {
				return nil, fmt.Errorf("rename response violates S43: %w", err)
			}
			return withBudget, nil
		}
	}
	return nil, fmt.Errorf("rename response byte count did not stabilize: %w", ErrProviderResponse)
}

func (service *Service) outputBudgetResponse(ctx context.Context, requestID model.UUID, maxBytes int64) ([]byte, error) {
	if ctx == nil {
		return nil, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gap := response.Gap{
		Code: "BUDGET_TOO_SMALL", Reason: "The complete rename page and MCP framing exceed the requested output budget.",
		Count: nil, SourceRefs: []response.SourceRef{},
	}
	limitReason := "OUTPUT_BYTE_BUDGET"
	responseValue := RenameResponse{
		SchemaVersion: 1, RequestID: requestID, Status: "error", Scope: nil,
		Freshness: response.Freshness{
			Status: string(snapshot.FreshnessNotChecked), Scope: string(snapshot.FreshnessNone), CheckedAt: nil,
			BufferScope: "disk_only", Consistency: string(snapshot.FreshnessCapturedBytesWithObservedStability),
			ChangedCount: nil, SourceView: string(snapshot.FreshnessIndexedSnapshot),
		},
		Coverage:  response.Coverage{Status: "unknown", Capabilities: []response.Capability{}, Gaps: []response.Gap{gap}, ScopeDescription: "Rename preview was not delivered within the requested output budget."},
		Readiness: "blocked", Data: nil,
		Pagination: response.Pagination{
			Truncated: true, EnumerationComplete: false, ReturnedCount: 0,
			RequiredPending: nil, OptionalPending: nil, NextCursor: nil, LimitReason: &limitReason,
		},
		Budget: response.Budget{
			MaxOutputBytes: maxBytes, SerializedBytes: 0, TokenCount: nil, TokenEstimate: nil,
			TokenizerID: nil, EstimateOnly: false, DeliveryMode: service.deliveryMode,
		},
		Warnings: []response.Gap{}, Error: &response.Error{
			Code: "BUDGET_TOO_SMALL", Message: "Increase max_output_bytes and retry the same rename request or continuation.",
			Retryable: true, SuggestedAction: "expand", Details: []response.Gap{gap},
		},
	}
	for attempts := 0; attempts < 4; attempts++ {
		raw, err := json.Marshal(responseValue)
		if err != nil {
			return nil, fmt.Errorf("serialize rename budget response: %w", err)
		}
		responseValue.Budget.SerializedBytes = int64(len(raw))
		withBudget, err := json.Marshal(responseValue)
		if err != nil {
			return nil, fmt.Errorf("serialize rename budget response count: %w", err)
		}
		if int64(len(withBudget)) > maxBytes || mcpDualUpperBound(withBudget) > maxBytes {
			return nil, ErrOutputBudget
		}
		if int64(len(withBudget)) == responseValue.Budget.SerializedBytes {
			if err := service.validator.ValidateResponse(operation, withBudget); err != nil {
				return nil, fmt.Errorf("rename budget response violates S43: %w", err)
			}
			return withBudget, nil
		}
	}
	return nil, ErrProviderResponse
}

// mcpDualUpperBound includes the structured response and its JSON text copy,
// escaped for the MCP wire envelope, plus a fixed allowance for framing.
func mcpDualUpperBound(raw []byte) int64 {
	length := int64(len(raw))
	escaped := int64(bytes.Count(raw, []byte{'"'}) + bytes.Count(raw, []byte{'\\'}))
	maximum := int64(^uint64(0) >> 1)
	if length > (maximum-escaped-512)/2 {
		return maximum
	}
	return 2*length + escaped + 512
}

func (service *Service) pageSize(maxBytes int64) int {
	pageSize := int((maxBytes - 8192) / service.pageBytesPerEdit)
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > lsp.MaxRenameEditPage || pageSize > 512 {
		pageSize = 512
	}
	return pageSize
}

func (service *Service) resultTokenIdentity(identity lsp.ResultIdentity) (cursor.CursorIdentity, error) {
	requestHash, err := model.HashValue("rename-result-token-v1", map[string]any{
		"engine_schema": identity.EngineSchema, "version": int64(1),
	})
	if err != nil {
		return cursor.CursorIdentity{}, err
	}
	return cursor.CursorIdentity{
		Purpose: resultTokenPurpose, Scope: identity.Scope, PrincipalID: identity.PrincipalID,
		AllowedBindingID: identity.AllowedBindingID, RequestHash: requestHash, OrderVersion: resultTokenVersion,
	}, nil
}

func (service *Service) issueResultToken(identity lsp.ResultIdentity, ref lsp.ResultRef, pageSize int, maxBytes int64, mask uint8, preserveContracts bool) (string, error) {
	cursorIdentity, err := service.resultTokenIdentity(identity)
	if err != nil {
		return "", err
	}
	resultID, digest := ref.ResultID, ref.Digest
	progress := cursor.CursorProgress{
		ResultID: &resultID, ResultDigest: &digest, Position: ref.EditCount,
		LastKey: tokenState(ref.Expires, pageSize, maxBytes, mask, preserveContracts),
	}
	return service.cursors.Issue(cursorIdentity, progress)
}

func (service *Service) verifyResultToken(token string, identity lsp.ResultIdentity) (lsp.ResultRef, int, int64, uint8, bool, error) {
	cursorIdentity, err := service.resultTokenIdentity(identity)
	if err != nil {
		return lsp.ResultRef{}, 0, 0, 0, false, err
	}
	progress, err := service.cursors.Verify(token, cursorIdentity)
	if err != nil || progress.ResultID == nil || progress.ResultDigest == nil || progress.Position < 0 || progress.Position > lsp.MaxRenameResultEdits {
		return lsp.ResultRef{}, 0, 0, 0, false, cursor.ErrCursorInvalid
	}
	expires, pageSize, maxBytes, mask, preserveContracts, err := parseTokenState(progress.LastKey)
	if err != nil {
		return lsp.ResultRef{}, 0, 0, 0, false, err
	}
	return lsp.ResultRef{ResultID: *progress.ResultID, Digest: *progress.ResultDigest, Expires: expires, EditCount: progress.Position}, pageSize, maxBytes, mask, preserveContracts, nil
}

func requireCurrent(validator PreconditionValidator, ctx context.Context, request validate.Request) error {
	result, err := validator.Validate(ctx, request)
	if err != nil {
		return fmt.Errorf("full-workspace precondition validation: %w", err)
	}
	if !currentFreshness(result) {
		return ErrPreconditions
	}
	return nil
}

func currentFreshness(result snapshot.ValidationResult) bool {
	return result.EditPreconditionValid && result.Freshness.Status == snapshot.FreshnessObservedCurrent &&
		result.Freshness.Scope == snapshot.FreshnessWorkspace && result.Freshness.BufferScope == "disk_only" &&
		result.Freshness.Consistency == snapshot.FreshnessCapturedBytesWithObservedStability &&
		result.Freshness.SourceView == snapshot.FreshnessIndexedSnapshot && result.Freshness.ChangedCount != nil && *result.Freshness.ChangedCount == 0
}

func validateResolvedInitial(resolved ResolvedInitial, selector Selector, identity lsp.ResultIdentity) error {
	request := resolved.Rename
	if !validIdentity(identity) || identity.EngineSchema != lsp.EngineSchemaVersion ||
		identity.Scope.Kind != "single" || len(identity.Scope.Members) != 1 || len(identity.Scope.UnavailableMembers) != 0 ||
		!sameScope(identity.Scope, request.Snapshot.Scope) || !sameScope(identity.Scope, resolved.Validation.ExpectedScopeVector) ||
		!request.Snapshot.Complete || request.Snapshot.Root == "" || !resolved.Validation.ExpectedComplete ||
		request.Source.Path != selector.Path || request.Source.ContentHash != selector.ContentHash ||
		model.SHA256(resolved.SelectedBytes) != selector.ContentHash || len(resolved.SelectedBytes) == 0 {
		return ErrIdentityChanged
	}
	member := identity.Scope.Members[0]
	if request.Source.WorktreeID != member.WorktreeID || request.Source.ProfileID != member.ProfileID || request.Source.GenerationID != member.GenerationID ||
		request.Profile.ID != member.ProfileID || request.Profile.ProviderDigest != resolved.Validation.Expected.ProviderDigest ||
		resolved.Validation.Expected.WorktreeID != member.WorktreeID || resolved.Validation.Expected.ProfileID != member.ProfileID ||
		resolved.Validation.Expected.SnapshotDigest != member.SnapshotDigest || resolved.Validation.Expected.PolicyDigest != member.PolicyDigest {
		return ErrIdentityChanged
	}
	if !snapshotMatchesManifest(request.Snapshot, resolved.Validation.Expected) {
		return fmt.Errorf("LSP snapshot does not match captured manifest: %w", ErrIdentityChanged)
	}
	selectedRange, _, err := lsp.IdentifierRangeAt(resolved.SelectedBytes, selector.Path, selector.ByteOffset)
	if err != nil || selectedRange != request.Source.Range || request.Source.Range.ValidateAgainst(resolved.SelectedBytes) != nil {
		return fmt.Errorf("selector does not identify the exact captured Go identifier: %w", ErrInvalidRequest)
	}
	manifestHash, found := capturedHash(resolved.Validation.Expected, selector.Path)
	if !found || manifestHash != selector.ContentHash {
		return ErrIdentityChanged
	}
	preconditionFound := false
	for _, source := range resolved.Validation.SourcePreconditions {
		if source.Path == selector.Path && source.ContentHash == selector.ContentHash {
			preconditionFound = true
			break
		}
	}
	if !preconditionFound || resolved.Validation.Expected.Validate() != nil {
		return ErrInvalidRequest
	}
	return nil
}

func snapshotMatchesManifest(workspace lsp.WorkspaceSnapshot, manifest model.CaptureManifest) bool {
	captured := make(map[string]model.Hash)
	for _, file := range manifest.Files {
		if file.Status != "captured" {
			continue
		}
		if file.ContentHash == nil {
			return false
		}
		captured[file.Path] = *file.ContentHash
	}
	if len(captured) != len(workspace.Files) {
		return false
	}
	for _, file := range workspace.Files {
		hash, ok := captured[file.Path]
		if !ok || hash != file.ContentHash {
			return false
		}
		delete(captured, file.Path)
	}
	return len(captured) == 0
}

func validateProviderPreview(preview lsp.RenamePreview, requirePrepared bool) error {
	if preview.Applied || preview.SourceMutationCount != 0 || !preview.ValidationRequired || preview.ResultRef.Expires < 1 ||
		preview.ResultRef.EditCount < 0 || preview.ResultRef.EditCount > lsp.MaxRenameResultEdits ||
		int64(len(preview.Edits)) > preview.ResultRef.EditCount || preview.Position < 0 || preview.Position > preview.ResultRef.EditCount ||
		preview.HasMore != (preview.Position < preview.ResultRef.EditCount) ||
		(preview.HasMore && preview.NextCursor == "") || (!preview.HasMore && preview.NextCursor != "") {
		return ErrProviderResponse
	}
	if _, err := model.ParseUUID(string(preview.ResultRef.ResultID)); err != nil {
		return ErrProviderResponse
	}
	if _, err := model.ParseHash(string(preview.ResultRef.Digest)); err != nil {
		return ErrProviderResponse
	}
	if requirePrepared && (preview.Prepared.Placeholder == "" || preview.Prepared.Range.StartByte < 0 || preview.Prepared.Range.EndByte <= preview.Prepared.Range.StartByte) {
		return ErrProviderResponse
	}
	return nil
}

func validateEditsAgainstManifest(edits []json.RawMessage, manifest model.CaptureManifest, member model.ScopeMember) error {
	for _, raw := range edits {
		var edit lsp.RenameEdit
		if json.Unmarshal(raw, &edit) != nil {
			return ErrProviderResponse
		}
		hash, found := capturedHash(manifest, edit.Path)
		if !found || hash != edit.ContentHash || edit.Path == "" || model.ValidateRelativePath(edit.Path) != nil {
			return ErrIdentityChanged
		}
		if member.WorktreeID != manifest.WorktreeID || member.ProfileID != manifest.ProfileID {
			return ErrIdentityChanged
		}
	}
	return nil
}

func capturedHash(manifest model.CaptureManifest, path string) (model.Hash, bool) {
	for _, file := range manifest.Files {
		if file.Path == path && file.ContentHash != nil {
			return *file.ContentHash, true
		}
	}
	return "", false
}

func validHash(value model.Hash) bool {
	_, err := model.ParseHash(string(value))
	return err == nil
}

func normalizeWarnings(codes []string) ([]string, error) {
	result := make([]string, 0, len(codes))
	seen := make(map[string]struct{}, len(codes))
	for _, code := range codes {
		if code != warningRuntimeLookup && code != warningSerialization {
			return nil, ErrProviderResponse
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		result = append(result, code)
	}
	return result, nil
}

func warningGap(code string) response.Gap {
	reason := "Review string-based lookup sites before applying the rename."
	if code == warningSerialization {
		reason = "Renaming this exported field may change its implicit JSON property name."
	}
	return response.Gap{Code: code, Reason: reason, Count: nil, SourceRefs: []response.SourceRef{}}
}

func warningMask(codes []string) uint8 {
	var mask uint8
	for _, code := range codes {
		mask |= warningBit(code)
	}
	return mask
}

func warningBit(code string) uint8 {
	switch code {
	case warningRuntimeLookup:
		return 1
	case warningSerialization:
		return 2
	default:
		return 0
	}
}

func warningsFromMask(mask uint8) []string {
	codes := make([]string, 0, 2)
	if mask&1 != 0 {
		codes = append(codes, warningRuntimeLookup)
	}
	if mask&2 != 0 {
		codes = append(codes, warningSerialization)
	}
	return codes
}

func tokenState(expires int64, pageSize int, maxBytes int64, mask uint8, preserveContracts bool) string {
	preserve := 0
	if preserveContracts {
		preserve = 1
	}
	return strconv.FormatInt(expires, 10) + ":" + strconv.Itoa(pageSize) + ":" + strconv.FormatInt(maxBytes, 10) + ":" + strconv.Itoa(int(mask)) + ":" + strconv.Itoa(preserve)
}

func parseTokenState(value string) (int64, int, int64, uint8, bool, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 5 {
		return 0, 0, 0, 0, false, cursor.ErrCursorInvalid
	}
	expires, errExpires := strconv.ParseInt(parts[0], 10, 64)
	pageSize, errPage := strconv.Atoi(parts[1])
	maxBytes, errBudget := strconv.ParseInt(parts[2], 10, 64)
	mask, errMask := strconv.ParseUint(parts[3], 10, 8)
	preserve, errPreserve := strconv.ParseUint(parts[4], 10, 1)
	if errExpires != nil || errPage != nil || errBudget != nil || errMask != nil || expires < 1 ||
		errPreserve != nil || pageSize < 1 || pageSize > 512 || mask > 3 {
		return 0, 0, 0, 0, false, cursor.ErrCursorInvalid
	}
	if _, err := requestedOutputBudget(&maxBytes); err != nil {
		return 0, 0, 0, 0, false, cursor.ErrCursorInvalid
	}
	return expires, pageSize, maxBytes, uint8(mask), preserve == 1, nil
}

func requestedOutputBudget(value *int64) (int64, error) {
	if value == nil {
		return defaultOutputBytes, nil
	}
	if *value < response.MinMaxOutputBytes || *value > maxOutputBytes {
		return 0, ErrInvalidRequest
	}
	return *value, nil
}

func responseFreshness(freshness snapshot.Freshness) response.Freshness {
	var checkedAt *string
	if freshness.CheckedAt != nil && !freshness.CheckedAt.IsZero() {
		value := freshness.CheckedAt.UTC().Format(time.RFC3339Nano)
		checkedAt = &value
	}
	return response.Freshness{
		Status: string(freshness.Status), Scope: string(freshness.Scope), CheckedAt: checkedAt,
		BufferScope: freshness.BufferScope, Consistency: string(freshness.Consistency),
		ChangedCount: freshness.ChangedCount, SourceView: string(freshness.SourceView),
	}
}

func sameIdentity(left, right lsp.ResultIdentity) bool {
	return sameScope(left.Scope, right.Scope) && left.PrincipalID == right.PrincipalID &&
		left.AllowedBindingID == right.AllowedBindingID && left.EngineSchema == right.EngineSchema
}

func validIdentity(identity lsp.ResultIdentity) bool {
	return identity.Scope.Validate() == nil && len(identity.PrincipalID) > 0 && len(identity.PrincipalID) <= 256 &&
		len(identity.AllowedBindingID) > 0 && len(identity.AllowedBindingID) <= 256 &&
		len(identity.EngineSchema) > 0 && len(identity.EngineSchema) <= 64 &&
		utf8.ValidString(string(identity.PrincipalID)) && utf8.ValidString(string(identity.AllowedBindingID)) && utf8.ValidString(identity.EngineSchema)
}

func sameResultRef(left, right lsp.ResultRef) bool {
	return left.ResultID == right.ResultID && left.Digest == right.Digest && left.Expires == right.Expires && left.EditCount == right.EditCount
}

func sameScope(left, right model.ScopeVector) bool {
	leftDigest, leftErr := left.ComputeDigest()
	rightDigest, rightErr := right.ComputeDigest()
	return leftErr == nil && rightErr == nil && left.Validate() == nil && right.Validate() == nil &&
		leftDigest == rightDigest && left.VectorDigest == right.VectorDigest && left.CatalogIncarnation == right.CatalogIncarnation
}

func cloneScope(value model.ScopeVector) *model.ScopeVector {
	copyValue := value
	copyValue.Members = append(make([]model.ScopeMember, 0, len(value.Members)), value.Members...)
	copyValue.UnavailableMembers = append(make([]model.UnavailableMember, 0, len(value.UnavailableMembers)), value.UnavailableMembers...)
	return &copyValue
}

type ProposedEdit struct {
	SourceRef response.SourceRef `json:"source_ref"`
	NewText   string             `json:"new_text"`
}

type RenameData struct {
	Operation           string         `json:"operation"`
	Applied             bool           `json:"applied"`
	SourceMutationCount int64          `json:"source_mutation_count"`
	ValidationRequired  bool           `json:"validation_required"`
	Edits               []ProposedEdit `json:"edits"`
	Conflicts           []response.Gap `json:"conflicts"`
	PreviewComplete     bool           `json:"preview_complete"`
	ResultToken         string         `json:"result_token"`
	ResultID            model.UUID     `json:"result_id"`
	ResultDigest        model.Hash     `json:"result_digest"`
	TotalEdits          int64          `json:"total_edits"`
	ComputationComplete bool           `json:"computation_complete"`
}

type RenameResponse struct {
	SchemaVersion int64               `json:"schema_version"`
	RequestID     model.UUID          `json:"request_id"`
	Status        string              `json:"status"`
	Scope         *model.ScopeVector  `json:"scope"`
	Freshness     response.Freshness  `json:"freshness"`
	Coverage      response.Coverage   `json:"coverage"`
	Readiness     string              `json:"readiness"`
	Data          *RenameData         `json:"data"`
	Pagination    response.Pagination `json:"pagination"`
	Budget        response.Budget     `json:"budget"`
	Warnings      []response.Gap      `json:"warnings"`
	Error         *response.Error     `json:"error"`
}
