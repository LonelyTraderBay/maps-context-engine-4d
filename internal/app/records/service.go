// Package records exposes durable worktree records with source status derived
// from the current policy and retained index snapshots.
package records

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/history"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

const (
	defaultListLimit = int64(20)
	maxListLimit     = int64(100)
	maxPageDataBytes = 900 << 10
	maxCursorBytes   = 32 << 10
)

var (
	ErrInvalidRequest       = errors.New("invalid records request")
	ErrStalePage            = errors.New("records page is stale")
	ErrStaleObservation     = errors.New("records changed during the query")
	ErrSourceNotCurrent     = errors.New("record sources are not current under the active policy")
	ErrPrincipalUnavailable = errors.New("authenticated record principal is unavailable")
	ErrPageTooLarge         = errors.New("records page exceeds the CLI output budget")
)

type Store interface {
	WorktreeID() model.UUID
	Incarnation() model.UUID
	List(context.Context, history.ListFilter) (sqlite.RecordPage, error)
	Read(context.Context, model.UUID, *int64) (history.Version, int64, error)
	RecordEpoch(context.Context) (int64, error)
	Propose(context.Context, model.Identifier, history.ProposeInput) (history.MutationResult, error)
	Review(context.Context, model.Identifier, history.ReviewInput) (history.MutationResult, error)
}

type Index interface {
	Incarnation() model.UUID
	WithPublishedSnapshot(context.Context, model.Hash, model.UUID, func(*sqlite.ReadSnapshot) error) error
}

type PolicyState struct {
	WorktreeID         model.UUID
	CatalogIncarnation model.UUID
	Digest             model.Hash
	Epoch              int64
	BindingRevision    int64
	Status             string
	CanonicalRoot      string
	Config             catalog.PolicyConfig
}

type PolicyReader interface {
	CurrentPolicy(context.Context, model.UUID) (PolicyState, error)
}

type PrincipalProvider interface {
	Principal(context.Context) (model.Identifier, error)
}

type Options struct {
	WorktreeID       model.UUID
	Records          Store
	Index            Index
	IndexUnavailable bool
	Policies         PolicyReader
	Principals       PrincipalProvider
}

type Service struct {
	worktree         model.UUID
	records          Store
	index            Index
	indexUnavailable bool
	policies         PolicyReader
	principals       PrincipalProvider
}

type ListRequest struct {
	Kind         history.Kind
	Status       history.Status
	Limit        int64
	Cursor       string
	ProfileID    model.Hash
	GenerationID model.UUID
}

type ListResult struct {
	Records     []history.Summary `json:"records"`
	RecordEpoch int64             `json:"record_epoch"`
	NextCursor  string            `json:"next_cursor,omitempty"`
	HasMore     bool              `json:"has_more"`
}

type ReadResult struct {
	Record      history.Summary `json:"record"`
	RecordEpoch int64           `json:"record_epoch"`
}

type MutationResult struct {
	Record      history.Summary `json:"record"`
	RecordEpoch int64           `json:"record_epoch"`
	Replayed    bool            `json:"replayed"`
}

type pageCursor struct {
	FormatVersion      int64      `json:"format_version"`
	WorktreeID         model.UUID `json:"worktree_id"`
	RecordsIncarnation model.UUID `json:"records_incarnation"`
	RecordEpoch        int64      `json:"record_epoch"`
	FilterHash         model.Hash `json:"filter_hash"`
	AfterRecordID      model.UUID `json:"after_record_id"`
}

func NewService(options Options) (*Service, error) {
	worktreeID, err := model.ParseUUID(string(options.WorktreeID))
	if err != nil || worktreeID != options.WorktreeID {
		return nil, fmt.Errorf("worktree ID is invalid: %w", ErrInvalidRequest)
	}
	if options.Records != nil && options.Records.WorktreeID() != worktreeID {
		return nil, fmt.Errorf("records store belongs to another worktree: %w", ErrInvalidRequest)
	}
	if options.Index != nil && options.Index.Incarnation() == "" {
		return nil, fmt.Errorf("index identity is unavailable: %w", ErrInvalidRequest)
	}
	return &Service{worktree: worktreeID, records: options.Records, index: options.Index,
		indexUnavailable: options.IndexUnavailable, policies: options.Policies, principals: options.Principals}, nil
}

func (service *Service) List(ctx context.Context, request ListRequest) (ListResult, error) {
	if ctx == nil || service == nil {
		return ListResult{}, ErrInvalidRequest
	}
	if err := validateListRequest(request); err != nil {
		return ListResult{}, err
	}
	if request.Limit == 0 {
		request.Limit = defaultListLimit
	}
	if service.records == nil {
		return ListResult{Records: []history.Summary{}, RecordEpoch: 0}, nil
	}
	filterHash, err := listFilterHash(request)
	if err != nil {
		return ListResult{}, err
	}
	var continuation *pageCursor
	if request.Cursor != "" {
		decoded, err := decodePageCursor(request.Cursor)
		if err != nil || decoded.WorktreeID != service.worktree || decoded.RecordsIncarnation != service.records.Incarnation() || decoded.FilterHash != filterHash {
			return ListResult{}, fmt.Errorf("cursor identity or filter changed: %w", ErrStalePage)
		}
		epoch, err := service.records.RecordEpoch(ctx)
		if err != nil {
			return ListResult{}, err
		}
		if decoded.RecordEpoch != epoch {
			return ListResult{}, ErrStalePage
		}
		continuation = &decoded
	}

	for attempt := 0; attempt < 2; attempt++ {
		filter := history.ListFilter{Kind: request.Kind, Status: request.Status, Limit: request.Limit}
		if continuation != nil {
			filter.AfterRecordID = continuation.AfterRecordID
		}
		page, err := service.records.List(ctx, filter)
		if err != nil {
			return ListResult{}, err
		}
		if continuation != nil && page.RecordEpoch != continuation.RecordEpoch {
			return ListResult{}, ErrStalePage
		}
		policyState, policyKnown, err := service.readPolicy(ctx)
		if err != nil {
			return ListResult{}, err
		}
		items := make([]history.Summary, 0, len(page.Items))
		for _, record := range page.Items {
			if !matchesSourceScope(record.SourceRefs, request.ProfileID, request.GenerationID) {
				continue
			}
			provenance, err := service.provenance(ctx, record, policyState, policyKnown)
			if err != nil {
				return ListResult{}, err
			}
			summary := summarize(record, provenance)
			if request.Status != "" && summary.EffectiveStatus != request.Status {
				continue
			}
			items = append(items, summary)
		}
		if policyKnown {
			if err := service.checkPolicyStable(ctx, policyState); err != nil {
				if errors.Is(err, ErrStaleObservation) && attempt == 0 && continuation == nil {
					continue
				}
				return ListResult{}, err
			}
		}
		epoch, err := service.records.RecordEpoch(ctx)
		if err != nil {
			return ListResult{}, err
		}
		if epoch != page.RecordEpoch {
			if continuation == nil && attempt == 0 {
				continue
			}
			return ListResult{}, ErrStalePage
		}
		if ctx.Err() != nil {
			return ListResult{}, ctx.Err()
		}

		hasMore := page.HasMore
		after := page.NextAfterID
		trimmed := false
		for len(items) > 0 {
			raw, err := json.Marshal(items)
			if err != nil {
				return ListResult{}, fmt.Errorf("encode records list for output sizing: %w", err)
			}
			if len(raw) <= maxPageDataBytes {
				break
			}
			items = items[:len(items)-1]
			trimmed = true
		}
		if trimmed {
			if len(items) == 0 {
				return ListResult{}, ErrPageTooLarge
			}
			after = items[len(items)-1].RecordID
			hasMore = true
		}
		result := ListResult{Records: items, RecordEpoch: page.RecordEpoch, HasMore: hasMore}
		if hasMore {
			if after == "" {
				return ListResult{}, fmt.Errorf("records page did not advance its keyset: %w", history.ErrRecordStoreCorrupted)
			}
			result.NextCursor, err = encodePageCursor(pageCursor{FormatVersion: 1, WorktreeID: service.worktree,
				RecordsIncarnation: service.records.Incarnation(), RecordEpoch: page.RecordEpoch, FilterHash: filterHash, AfterRecordID: after})
			if err != nil {
				return ListResult{}, err
			}
		}
		return result, nil
	}
	return ListResult{}, ErrStalePage
}

func (service *Service) Read(ctx context.Context, recordID model.UUID, version *int64, profileID model.Hash, generationID model.UUID) (ReadResult, error) {
	if ctx == nil || service == nil || service.records == nil {
		return ReadResult{}, history.ErrRecordNotFound
	}
	if err := validateSourceScope(profileID, generationID); err != nil {
		return ReadResult{}, err
	}
	record, epoch, err := service.records.Read(ctx, recordID, version)
	if err != nil {
		return ReadResult{}, err
	}
	if !matchesSourceScope(record.SourceRefs, profileID, generationID) {
		return ReadResult{}, history.ErrRecordNotFound
	}
	policyState, policyKnown, err := service.readPolicy(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	provenance, err := service.provenance(ctx, record, policyState, policyKnown)
	if err != nil {
		return ReadResult{}, err
	}
	if policyKnown {
		if err := service.checkPolicyStable(ctx, policyState); err != nil {
			return ReadResult{}, err
		}
	}
	currentEpoch, err := service.records.RecordEpoch(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	if currentEpoch != epoch {
		return ReadResult{}, ErrStalePage
	}
	return ReadResult{Record: summarize(record, provenance), RecordEpoch: epoch}, nil
}

func (service *Service) Propose(ctx context.Context, input history.ProposeInput) (MutationResult, error) {
	if service == nil || service.records == nil {
		return MutationResult{}, ErrPrincipalUnavailable
	}
	principal, err := service.principal(ctx)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := service.records.Propose(ctx, principal, input)
	if err != nil {
		return MutationResult{}, err
	}
	policyState, policyKnown, err := service.readPolicy(ctx)
	if err != nil {
		return MutationResult{}, err
	}
	provenance, err := service.provenance(ctx, result.Version, policyState, policyKnown)
	if err != nil {
		return MutationResult{}, err
	}
	epoch, err := service.records.RecordEpoch(ctx)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Record: summarize(result.Version, provenance), RecordEpoch: epoch, Replayed: result.Replayed}, nil
}

func (service *Service) Review(ctx context.Context, input history.ReviewInput) (MutationResult, error) {
	if service == nil || service.records == nil {
		return MutationResult{}, ErrPrincipalUnavailable
	}
	principal, err := service.principal(ctx)
	if err != nil {
		return MutationResult{}, err
	}
	if input.Action == history.ReviewApprove {
		current, _, err := service.records.Read(ctx, input.RecordID, nil)
		if err != nil {
			return MutationResult{}, err
		}
		if current.Version != input.ExpectedVersion {
			return MutationResult{}, history.ErrRecordConflict
		}
		policyState, policyKnown, err := service.readPolicy(ctx)
		if err != nil {
			return MutationResult{}, err
		}
		provenance, err := service.provenance(ctx, current, policyState, policyKnown)
		if err != nil {
			return MutationResult{}, err
		}
		if provenance != history.ProvenanceRetainedSnapshot {
			return MutationResult{}, ErrSourceNotCurrent
		}
		if policyKnown {
			if err := service.checkPolicyStable(ctx, policyState); err != nil {
				return MutationResult{}, err
			}
		}
	}
	result, err := service.records.Review(ctx, principal, input)
	if err != nil {
		return MutationResult{}, err
	}
	policyState, policyKnown, err := service.readPolicy(ctx)
	if err != nil {
		return MutationResult{}, err
	}
	provenance, err := service.provenance(ctx, result.Version, policyState, policyKnown)
	if err != nil {
		return MutationResult{}, err
	}
	epoch, err := service.records.RecordEpoch(ctx)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Record: summarize(result.Version, provenance), RecordEpoch: epoch, Replayed: result.Replayed}, nil
}

func (service *Service) principal(ctx context.Context) (model.Identifier, error) {
	if ctx == nil || service.principals == nil {
		return "", ErrPrincipalUnavailable
	}
	principal, err := service.principals.Principal(ctx)
	if err != nil {
		return "", err
	}
	if err := history.ValidatePrincipal(principal); err != nil {
		return "", ErrPrincipalUnavailable
	}
	return principal, nil
}

func (service *Service) provenance(ctx context.Context, record history.Version, current PolicyState, policyKnown bool) (history.ProvenanceStatus, error) {
	if len(record.SourceRefs) == 0 {
		return history.ProvenanceUnverified, nil
	}
	if service.indexUnavailable {
		return history.ProvenanceUnverified, nil
	}
	if service.index == nil {
		return history.ProvenanceSnapshotExpired, nil
	}
	if !policyKnown {
		return history.ProvenanceUnverified, nil
	}
	statuses := make([]history.ProvenanceStatus, 0, len(record.SourceRefs))
	for _, ref := range record.SourceRefs {
		if ref.WorktreeID != service.worktree || current.Status != "registered" || !policyAllowsRoot(current) || !policyAllowsPath(current.Config, ref.Path) {
			statuses = append(statuses, history.ProvenanceAccessDenied)
			continue
		}
		status, err := service.sourceStatus(ctx, ref, current)
		if err != nil {
			return "", err
		}
		statuses = append(statuses, status)
	}
	return history.AggregateProvenance(statuses), nil
}

func (service *Service) sourceStatus(ctx context.Context, ref model.SourceRef, current PolicyState) (history.ProvenanceStatus, error) {
	status := history.ProvenanceUnverified
	err := service.index.WithPublishedSnapshot(ctx, ref.ProfileID, ref.GenerationID, func(snapshot *sqlite.ReadSnapshot) error {
		if snapshot.Generation.Key.ProfileID != ref.ProfileID || snapshot.Generation.Key.GenerationID != ref.GenerationID {
			status = history.ProvenanceAccessDenied
			return nil
		}
		if snapshot.Generation.PolicyDigest != current.Digest || snapshot.Generation.PolicyEpoch != current.Epoch {
			status = history.ProvenanceAccessDenied
			return nil
		}
		row, err := snapshot.QueryRowContext(ctx, `SELECT content_hash,status,byte_length FROM files WHERE generation_id=? AND path=?`, string(ref.GenerationID), ref.Path)
		if err != nil {
			return err
		}
		var contentHash, fileStatus sql.NullString
		var byteLength sql.NullInt64
		if err := row.Scan(&contentHash, &fileStatus, &byteLength); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				status = history.ProvenanceSourceChanged
				return nil
			}
			return err
		}
		if fileStatus.String != "indexed" && fileStatus.String != "parse_error" {
			status = history.ProvenanceAccessDenied
			return nil
		}
		if !contentHash.Valid || contentHash.String != string(ref.ContentHash) || !byteLength.Valid || ref.Range.EndByte > byteLength.Int64 {
			status = history.ProvenanceSourceChanged
			return nil
		}
		status = history.ProvenanceRetainedSnapshot
		return nil
	})
	if errors.Is(err, sqlite.ErrGenerationUnavailable) {
		return history.ProvenanceSnapshotExpired, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return history.ProvenanceUnverified, nil
	}
	return status, nil
}

func (service *Service) readPolicy(ctx context.Context) (PolicyState, bool, error) {
	if service.policies == nil {
		return PolicyState{}, false, nil
	}
	state, err := service.policies.CurrentPolicy(ctx, service.worktree)
	if err != nil {
		if ctx.Err() != nil {
			return PolicyState{}, false, ctx.Err()
		}
		return PolicyState{}, false, nil
	}
	if err := validatePolicyState(service.worktree, state); err != nil {
		return PolicyState{}, false, nil
	}
	return state, true, nil
}

func (service *Service) checkPolicyStable(ctx context.Context, expected PolicyState) error {
	current, known, err := service.readPolicy(ctx)
	if err != nil {
		return err
	}
	if !known || current.Digest != expected.Digest || current.Epoch != expected.Epoch ||
		current.BindingRevision != expected.BindingRevision || current.Status != expected.Status || current.CanonicalRoot != expected.CanonicalRoot || current.CatalogIncarnation != expected.CatalogIncarnation {
		return ErrStaleObservation
	}
	return nil
}

func summarize(record history.Version, provenance history.ProvenanceStatus) history.Summary {
	refs := append([]model.SourceRef{}, record.SourceRefs...)
	return history.Summary{RecordID: record.RecordID, Version: record.Version, Kind: record.Kind,
		DeclaredStatus: record.DeclaredStatus, EffectiveStatus: history.EffectiveStatus(record.DeclaredStatus, provenance),
		ProvenanceStatus: provenance, SourceRefs: refs}
}

func validateListRequest(request ListRequest) error {
	if request.Kind != "" && !request.Kind.Valid() || request.Status != "" && !request.Status.ValidDeclared() && request.Status != history.StatusNeedsReview {
		return ErrInvalidRequest
	}
	if request.Limit != 0 && (request.Limit < 1 || request.Limit > maxListLimit) {
		return ErrInvalidRequest
	}
	return validateSourceScope(request.ProfileID, request.GenerationID)
}

func validateSourceScope(profileID model.Hash, generationID model.UUID) error {
	if profileID != "" {
		parsed, err := model.ParseHash(string(profileID))
		if err != nil || parsed != profileID {
			return ErrInvalidRequest
		}
	}
	if generationID != "" {
		parsed, err := model.ParseUUID(string(generationID))
		if err != nil || parsed != generationID || profileID == "" {
			return ErrInvalidRequest
		}
	}
	return nil
}

func matchesSourceScope(refs []model.SourceRef, profileID model.Hash, generationID model.UUID) bool {
	if profileID == "" {
		return true
	}
	if len(refs) == 0 {
		return true // Worktree-level decisions are not tied to one profile.
	}
	for _, ref := range refs {
		if ref.ProfileID == profileID && (generationID == "" || ref.GenerationID == generationID) {
			return true
		}
	}
	return false
}

func listFilterHash(request ListRequest) (model.Hash, error) {
	limit := request.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	return model.HashValue("records-list-filter-v1", map[string]any{
		"record_kind": string(request.Kind), "status": string(request.Status), "limit": limit,
		"profile_id": string(request.ProfileID), "generation_id": string(request.GenerationID),
	})
}

func encodePageCursor(cursor pageCursor) (string, error) {
	if cursor.FormatVersion != 1 || cursor.RecordEpoch < 0 || cursor.RecordEpoch > model.MaxSafeInteger {
		return "", ErrStalePage
	}
	if _, err := model.ParseUUID(string(cursor.WorktreeID)); err != nil {
		return "", ErrStalePage
	}
	if _, err := model.ParseUUID(string(cursor.RecordsIncarnation)); err != nil {
		return "", ErrStalePage
	}
	if _, err := model.ParseUUID(string(cursor.AfterRecordID)); err != nil {
		return "", ErrStalePage
	}
	if _, err := model.ParseHash(string(cursor.FilterHash)); err != nil {
		return "", ErrStalePage
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	canonical, _, err := history.CanonicalPayload(raw)
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(canonical)
	if len(token) > maxCursorBytes {
		return "", ErrStalePage
	}
	return token, nil
}

func decodePageCursor(token string) (pageCursor, error) {
	var cursor pageCursor
	if len(token) == 0 || len(token) > maxCursorBytes {
		return cursor, ErrStalePage
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) > 16<<10 {
		return cursor, ErrStalePage
	}
	canonical, _, err := history.CanonicalPayload(raw)
	if err != nil || !bytes.Equal(canonical, raw) {
		return cursor, ErrStalePage
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.FormatVersion != 1 || cursor.RecordEpoch < 0 || cursor.RecordEpoch > model.MaxSafeInteger {
		return pageCursor{}, ErrStalePage
	}
	if _, err := model.ParseUUID(string(cursor.WorktreeID)); err != nil {
		return pageCursor{}, ErrStalePage
	}
	if _, err := model.ParseUUID(string(cursor.RecordsIncarnation)); err != nil {
		return pageCursor{}, ErrStalePage
	}
	if _, err := model.ParseUUID(string(cursor.AfterRecordID)); err != nil {
		return pageCursor{}, ErrStalePage
	}
	if _, err := model.ParseHash(string(cursor.FilterHash)); err != nil {
		return pageCursor{}, ErrStalePage
	}
	return cursor, nil
}

func validatePolicyState(worktreeID model.UUID, state PolicyState) error {
	if state.WorktreeID != worktreeID || state.Status == "" || state.Epoch < 1 || state.Epoch > model.MaxSafeInteger || state.BindingRevision < 1 || state.BindingRevision > model.MaxSafeInteger {
		return ErrInvalidRequest
	}
	if _, err := model.ParseHash(string(state.Digest)); err != nil {
		return err
	}
	if _, err := model.ParseUUID(string(state.CatalogIncarnation)); err != nil {
		return err
	}
	computed, err := state.Config.Digest()
	if err != nil || computed != state.Digest {
		return ErrInvalidRequest
	}
	return nil
}

func policyAllowsRoot(state PolicyState) bool {
	if state.Status != "registered" || state.CanonicalRoot == "" {
		return false
	}
	root := strings.TrimRight(strings.ReplaceAll(state.CanonicalRoot, "\\", "/"), "/")
	for _, allowed := range state.Config.AllowedRoots {
		candidate := strings.TrimRight(strings.ReplaceAll(allowed, "\\", "/"), "/")
		if pathsEqual(root, candidate) {
			return true
		}
	}
	return false
}

func pathsEqual(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func policyAllowsPath(config catalog.PolicyConfig, value string) bool {
	if model.ValidateRelativePath(value) != nil {
		return false
	}
	for _, mode := range []paths.CaseMode{paths.CaseSensitive, paths.CaseInsensitive} {
		compiled, err := policy.New(config.DeniedGlobs, mode)
		if err != nil {
			return false
		}
		if !compiled.AllowsText(value) {
			return false
		}
		decision, err := compiled.CheckPath(value, false)
		if err != nil || decision.Denied {
			return false
		}
	}
	return true
}
