// Package lsp contains immutable, bounded result primitives for LSP operations.
package lsp

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
	"time"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	signedcursor "github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

const (
	RenameResultTTL       = 900 * time.Second
	MaxRenameResultEdits  = 10_000
	MaxRenameEditPage     = 1_000
	DefaultRenameEditPage = 100
	EngineSchemaVersion   = "mce4d-engine-v1"
)

var (
	ErrResultInvalid = errors.New("immutable LSP result is invalid")
	ErrResultExpired = errors.New("immutable LSP result has expired")
)

type ResultIdentity struct {
	Scope            model.ScopeVector
	PrincipalID      model.Identifier
	AllowedBindingID model.Identifier
	EngineSchema     string
}

type ResultRef struct {
	ResultID  model.UUID
	Digest    model.Hash
	Expires   int64
	EditCount int64
}

type EditPage struct {
	Edits      []json.RawMessage
	NextCursor string
	Position   int64
	HasMore    bool
}

type ResultStore struct {
	store resultSpool
	now   func() time.Time
}

type resultSpool interface {
	WriteRenameResultSpool(context.Context, model.UUID, []byte, model.Hash) error
	ReadRenameResultSpool(context.Context, model.UUID, model.Hash) ([]byte, error)
	RemoveRenameResultSpool(context.Context, model.UUID) error
}

type storedResult struct {
	Version          int64             `json:"version"`
	ResultID         model.UUID        `json:"result_id"`
	Scope            model.ScopeVector `json:"scope"`
	PrincipalID      model.Identifier  `json:"principal_id"`
	AllowedBindingID model.Identifier  `json:"allowed_binding_id"`
	EngineSchema     string            `json:"engine_schema"`
	CreatedAt        int64             `json:"created_at_unix"`
	ExpiresAt        int64             `json:"expires_at_unix"`
	EditCount        int64             `json:"edit_count"`
	Edits            []json.RawMessage `json:"edits"`
}

func NewResultStore(store resultSpool) (*ResultStore, error) {
	if store == nil {
		return nil, fmt.Errorf("invalid immutable result store bounds: %w", ErrResultInvalid)
	}
	return &ResultStore{store: store, now: time.Now}, nil
}

// Publish stores a result only after the LSP prepare and full post-validation
// have completed. Callers pass bounded edit objects, never an apply request.
func (resultStore *ResultStore) Publish(ctx context.Context, identity ResultIdentity, edits []json.RawMessage) (ResultRef, error) {
	if resultStore == nil || resultStore.store == nil || ctx == nil || resultStore.now == nil || validateResultIdentity(identity) != nil || edits == nil || len(edits) > MaxRenameResultEdits {
		return ResultRef{}, ErrResultInvalid
	}
	now := resultStore.now().UTC().Unix()
	if now < 0 || now > model.MaxSafeInteger-int64(RenameResultTTL/time.Second) {
		return ResultRef{}, ErrResultInvalid
	}
	canonicalEdits := make([]json.RawMessage, len(edits))
	for index, edit := range edits {
		canonical, err := canonicalEdit(edit)
		if err != nil {
			return ResultRef{}, fmt.Errorf("validate post-checked rename edit %d: %w", index, err)
		}
		canonicalEdits[index] = canonical
	}
	resultID, err := newResultUUID()
	if err != nil {
		return ResultRef{}, fmt.Errorf("create immutable rename result identity: %w", err)
	}
	envelope := storedResult{
		Version: 1, ResultID: resultID, Scope: identity.Scope, PrincipalID: identity.PrincipalID,
		AllowedBindingID: identity.AllowedBindingID, EngineSchema: identity.EngineSchema,
		CreatedAt: now, ExpiresAt: now + int64(RenameResultTTL/time.Second),
		EditCount: int64(len(canonicalEdits)), Edits: canonicalEdits,
	}
	raw, err := encodeStoredResult(envelope)
	if err != nil {
		return ResultRef{}, err
	}
	if int64(len(raw)) > sqlite.MaxRenameResultSpoolBytes {
		return ResultRef{}, fmt.Errorf("serialized rename result exceeds its fixed per-result cap: %w", sqlite.ErrAdmissionResourceLimit)
	}
	digest := model.SHA256(raw)
	if err := resultStore.store.WriteRenameResultSpool(ctx, resultID, raw, digest); err != nil {
		return ResultRef{}, err
	}
	return ResultRef{ResultID: resultID, Digest: digest, Expires: envelope.ExpiresAt, EditCount: envelope.EditCount}, nil
}

// Page reads the exact stored result and never reruns LSP. Its continuation is
// bound to result identity, digest, scope, principal, binding, and page size.
func (resultStore *ResultStore) Page(ctx context.Context, identity ResultIdentity, ref ResultRef, codec *signedcursor.CursorCodec, cursor string, limit int) (EditPage, error) {
	if resultStore == nil || resultStore.store == nil || ctx == nil || codec == nil || validateResultIdentity(identity) != nil ||
		limit < 1 || limit > MaxRenameEditPage || ref.EditCount < 0 || ref.EditCount > MaxRenameResultEdits || ref.Expires < 1 {
		return EditPage{}, ErrResultInvalid
	}
	if _, err := model.ParseUUID(string(ref.ResultID)); err != nil {
		return EditPage{}, ErrResultInvalid
	}
	if _, err := model.ParseHash(string(ref.Digest)); err != nil {
		return EditPage{}, ErrResultInvalid
	}
	cursorIdentity, err := resultPageCursorIdentity(identity, ref, limit)
	if err != nil {
		return EditPage{}, err
	}
	progress := signedcursor.CursorProgress{}
	if cursor != "" {
		progress, err = codec.Verify(cursor, cursorIdentity)
		if err != nil {
			return EditPage{}, err
		}
		if progress.ResultID == nil || progress.ResultDigest == nil || *progress.ResultID != ref.ResultID || *progress.ResultDigest != ref.Digest || progress.LastKey != strconv.FormatInt(progress.Position, 10) {
			return EditPage{}, signedcursor.ErrCursorScopeMismatch
		}
	}
	now := resultStore.now().UTC().Unix()
	if now < 0 || now >= ref.Expires {
		return EditPage{}, ErrResultExpired
	}
	raw, err := resultStore.store.ReadRenameResultSpool(ctx, ref.ResultID, ref.Digest)
	if err != nil {
		if errors.Is(err, sqlite.ErrRenameResultSpoolExpired) {
			return EditPage{}, ErrResultExpired
		}
		return EditPage{}, err
	}
	envelope, err := decodeStoredResult(raw)
	if err != nil {
		return EditPage{}, err
	}
	if !sameResultIdentity(envelope, identity) || envelope.ResultID != ref.ResultID || envelope.ExpiresAt != ref.Expires || envelope.EditCount != ref.EditCount || envelope.ExpiresAt-envelope.CreatedAt != int64(RenameResultTTL/time.Second) {
		return EditPage{}, signedcursor.ErrCursorScopeMismatch
	}
	if now >= envelope.ExpiresAt {
		return EditPage{}, ErrResultExpired
	}
	if progress.Position > envelope.EditCount {
		return EditPage{}, signedcursor.ErrCursorInvalid
	}
	start := int(progress.Position)
	end := start + limit
	if end > len(envelope.Edits) {
		end = len(envelope.Edits)
	}
	page := EditPage{Edits: append([]json.RawMessage(nil), envelope.Edits[start:end]...), Position: int64(end), HasMore: end < len(envelope.Edits)}
	if page.HasMore {
		resultID, digest := ref.ResultID, ref.Digest
		page.NextCursor, err = codec.Issue(cursorIdentity, signedcursor.CursorProgress{
			ResultID: &resultID, ResultDigest: &digest, LastKey: strconv.FormatInt(page.Position, 10), Position: page.Position,
		})
		if err != nil {
			return EditPage{}, err
		}
	}
	return page, nil
}

// PageWithPageSizeTransition verifies an existing cursor against its original
// page limit, then signs the same position for a smaller next page. This lets
// a consumer reduce a page to fit its byte budget without weakening cursor
// scope, result, digest, or page-limit binding.
func (resultStore *ResultStore) PageWithPageSizeTransition(ctx context.Context, identity ResultIdentity, ref ResultRef, codec *signedcursor.CursorCodec, cursor string, previousLimit, nextLimit int) (EditPage, error) {
	if resultStore == nil || resultStore.store == nil || ctx == nil || codec == nil || validateResultIdentity(identity) != nil ||
		nextLimit < 1 || nextLimit > previousLimit || previousLimit > MaxRenameEditPage ||
		ref.EditCount < 0 || ref.EditCount > MaxRenameResultEdits || ref.Expires < 1 {
		return EditPage{}, ErrResultInvalid
	}
	if _, err := model.ParseUUID(string(ref.ResultID)); err != nil {
		return EditPage{}, ErrResultInvalid
	}
	if _, err := model.ParseHash(string(ref.Digest)); err != nil {
		return EditPage{}, ErrResultInvalid
	}
	if cursor == "" || nextLimit == previousLimit {
		return resultStore.Page(ctx, identity, ref, codec, cursor, nextLimit)
	}
	previousIdentity, err := resultPageCursorIdentity(identity, ref, previousLimit)
	if err != nil {
		return EditPage{}, err
	}
	progress, err := codec.Verify(cursor, previousIdentity)
	if err != nil {
		return EditPage{}, err
	}
	if progress.ResultID == nil || progress.ResultDigest == nil || *progress.ResultID != ref.ResultID ||
		*progress.ResultDigest != ref.Digest || progress.LastKey != strconv.FormatInt(progress.Position, 10) {
		return EditPage{}, signedcursor.ErrCursorScopeMismatch
	}
	nextIdentity, err := resultPageCursorIdentity(identity, ref, nextLimit)
	if err != nil {
		return EditPage{}, err
	}
	rebound, err := codec.Issue(nextIdentity, progress)
	if err != nil {
		return EditPage{}, err
	}
	return resultStore.Page(ctx, identity, ref, codec, rebound, nextLimit)
}

func resultPageCursorIdentity(identity ResultIdentity, ref ResultRef, limit int) (signedcursor.CursorIdentity, error) {
	requestHash, err := model.HashValue("rename-result-page-v1", map[string]any{
		"result_id": string(ref.ResultID), "result_digest": string(ref.Digest), "limit": limit,
	})
	if err != nil {
		return signedcursor.CursorIdentity{}, err
	}
	return signedcursor.CursorIdentity{
		Purpose: "rename-page", Scope: identity.Scope, PrincipalID: identity.PrincipalID,
		AllowedBindingID: identity.AllowedBindingID, RequestHash: requestHash,
		OrderVersion: "rename-edit-index-v1",
	}, nil
}

func (resultStore *ResultStore) Remove(ctx context.Context, ref ResultRef) error {
	if resultStore == nil || resultStore.store == nil || ctx == nil {
		return ErrResultInvalid
	}
	return resultStore.store.RemoveRenameResultSpool(ctx, ref.ResultID)
}

func validateResultIdentity(identity ResultIdentity) error {
	if identity.Scope.Validate() != nil || !validResultText(string(identity.PrincipalID), 256) ||
		!validResultText(string(identity.AllowedBindingID), 256) || !validResultText(identity.EngineSchema, 64) {
		return ErrResultInvalid
	}
	return nil
}

func validResultText(value string, maxBytes int) bool {
	return len(value) > 0 && len(value) <= maxBytes && utf8.ValidString(value)
}

func canonicalEdit(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, ErrResultInvalid
	}
	value, err := contracts.DecodeJSON(raw)
	if err != nil {
		return nil, ErrResultInvalid
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, ErrResultInvalid
	}
	canonical, err := model.CanonicalJSON1(value)
	if err != nil {
		return nil, ErrResultInvalid
	}
	return json.RawMessage(canonical), nil
}

func encodeStoredResult(value storedResult) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode immutable LSP result: %w", ErrResultInvalid)
	}
	decoded, err := contracts.DecodeJSON(raw)
	if err != nil {
		return nil, ErrResultInvalid
	}
	canonical, err := model.CanonicalJSON1(decoded)
	if err != nil || int64(len(canonical)) > sqlite.MaxRenameResultSpoolBytes {
		return nil, fmt.Errorf("canonical immutable LSP result is invalid or oversized: %w", ErrResultInvalid)
	}
	return canonical, nil
}

func decodeStoredResult(raw []byte) (storedResult, error) {
	var result storedResult
	decoded, err := contracts.DecodeJSON(raw)
	if err != nil {
		return result, ErrResultInvalid
	}
	canonical, err := model.CanonicalJSON1(decoded)
	if err != nil || !bytes.Equal(canonical, raw) {
		return result, ErrResultInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return storedResult{}, ErrResultInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF || result.Version != 1 || result.EditCount != int64(len(result.Edits)) || result.Edits == nil {
		return storedResult{}, ErrResultInvalid
	}
	for index, edit := range result.Edits {
		canonicalEdit, err := canonicalEdit(edit)
		if err != nil || !bytes.Equal(canonicalEdit, edit) {
			return storedResult{}, fmt.Errorf("stored edit %d is not canonical: %w", index, ErrResultInvalid)
		}
	}
	if validateResultIdentity(ResultIdentity{Scope: result.Scope, PrincipalID: result.PrincipalID, AllowedBindingID: result.AllowedBindingID, EngineSchema: result.EngineSchema}) != nil ||
		result.CreatedAt < 0 || result.ExpiresAt-result.CreatedAt != int64(RenameResultTTL/time.Second) || result.EditCount < 0 || result.EditCount > MaxRenameResultEdits {
		return storedResult{}, ErrResultInvalid
	}
	if _, err := model.ParseUUID(string(result.ResultID)); err != nil {
		return storedResult{}, ErrResultInvalid
	}
	return result, nil
}

func sameResultIdentity(result storedResult, identity ResultIdentity) bool {
	left, leftErr := result.Scope.ComputeDigest()
	right, rightErr := identity.Scope.ComputeDigest()
	return leftErr == nil && rightErr == nil && left == right && result.Scope.CatalogIncarnation == identity.Scope.CatalogIncarnation &&
		result.PrincipalID == identity.PrincipalID && result.AllowedBindingID == identity.AllowedBindingID && result.EngineSchema == identity.EngineSchema
}

func newResultUUID() (model.UUID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return model.ParseUUID(string(encoded))
}
