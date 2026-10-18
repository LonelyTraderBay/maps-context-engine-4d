package cursor

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	CursorVersion        = int64(1)
	DefaultCursorTTL     = 900 * time.Second
	MaxCursorTTL         = 900 * time.Second
	maxCursorPayloadSize = 64 << 10
	maxCursorTokenSize   = 96 << 10
)

var (
	ErrCursorInvalid       = errors.New("continuation cursor is invalid")
	ErrCursorExpired       = errors.New("continuation cursor has expired")
	ErrCursorScopeMismatch = errors.New("continuation cursor scope or request changed")
)

// CursorIdentity is the immutable identity of one continuation stream. Scope
// carries the complete generation and incarnation vector; the other fields
// bind the caller, authorization, query, and algorithm-specific resolver.
type CursorIdentity struct {
	Purpose          string
	Scope            model.ScopeVector
	PrincipalID      model.Identifier
	AllowedBindingID model.Identifier
	RequestHash      model.Hash
	OrderVersion     string
	BindingID        *model.UUID
	ResolverDigest   *model.Hash
	MapID            *model.Hash
	SessionID        *model.UUID
	DocumentEpoch    int64
	HMRRevision      int64
	AuthEpoch        int64
}

// CursorProgress is bounded keyset/result progress, never a graph frontier.
type CursorProgress struct {
	ResultID     *model.UUID
	ResultDigest *model.Hash
	LastKey      string
	Position     int64
}

type cursorClaims struct {
	Version        int64             `json:"version"`
	Purpose        string            `json:"purpose"`
	Scope          model.ScopeVector `json:"scope"`
	PrincipalID    model.Identifier  `json:"principal_id"`
	AllowedBinding model.Identifier  `json:"allowed_binding_id"`
	RequestHash    model.Hash        `json:"request_hash"`
	OrderVersion   string            `json:"order_version"`
	BindingID      *model.UUID       `json:"binding_id"`
	ResolverDigest *model.Hash       `json:"resolver_digest"`
	MapID          *model.Hash       `json:"map_id"`
	SessionID      *model.UUID       `json:"session_id"`
	DocumentEpoch  int64             `json:"document_epoch"`
	HMRRevision    int64             `json:"hmr_revision"`
	AuthEpoch      int64             `json:"auth_epoch"`
	ResultID       *model.UUID       `json:"result_id"`
	ResultDigest   *model.Hash       `json:"result_digest"`
	LastKey        string            `json:"last_key"`
	Position       int64             `json:"position"`
	IssuedAtUnix   int64             `json:"issued_at_unix"`
	ExpiresAtUnix  int64             `json:"expires_at_unix"`
}

// CursorCodec signs purpose-separated, canonical JSON payloads with HMAC-SHA256.
// The caller must obtain key bytes from an owner-only key store.
type CursorCodec struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

func NewCursorCodec(key []byte, ttl time.Duration, now func() time.Time) (*CursorCodec, error) {
	if len(key) != sha256.Size || ttl < time.Second || ttl > MaxCursorTTL {
		return nil, fmt.Errorf("cursor key must be 32 bytes and TTL within 1..900 seconds: %w", ErrCursorInvalid)
	}
	if now == nil {
		now = time.Now
	}
	return &CursorCodec{key: append([]byte(nil), key...), ttl: ttl, now: now}, nil
}

// Issue signs one cursor only after its scope and identity have been validated.
// Callers must supply progress that moves beyond the last delivered item.
func (codec *CursorCodec) Issue(identity CursorIdentity, progress CursorProgress) (string, error) {
	if codec == nil || len(codec.key) != sha256.Size || codec.now == nil {
		return "", fmt.Errorf("cursor codec is not initialized: %w", ErrCursorInvalid)
	}
	if err := validateCursorIdentity(identity); err != nil {
		return "", err
	}
	if err := validateCursorProgress(progress); err != nil {
		return "", err
	}
	now := codec.now().UTC()
	claims := claimsFor(identity, progress)
	claims.Version = CursorVersion
	claims.IssuedAtUnix = now.Unix()
	claims.ExpiresAtUnix = now.Add(codec.ttl).Unix()
	if err := validateCursorClaims(claims, now); err != nil {
		return "", err
	}
	payload, err := canonicalCursorJSON(claims)
	if err != nil || len(payload) > maxCursorPayloadSize {
		return "", fmt.Errorf("cursor payload is invalid or too large: %w", ErrCursorInvalid)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := cursorMAC(codec.key, claims.Purpose, payload)
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(signature)
	if len(token) > maxCursorTokenSize {
		return "", fmt.Errorf("cursor token exceeds the bounded size: %w", ErrCursorInvalid)
	}
	return token, nil
}

// Verify authenticates before trusting claims, then checks expiry and exact
// request identity. It never substitutes a current generation or re-runs work.
func (codec *CursorCodec) Verify(token string, expected CursorIdentity) (CursorProgress, error) {
	if codec == nil || len(codec.key) != sha256.Size || codec.now == nil || len(token) == 0 || len(token) > maxCursorTokenSize {
		return CursorProgress{}, ErrCursorInvalid
	}
	parts := bytes.Split([]byte(token), []byte{'.'})
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 {
		return CursorProgress{}, ErrCursorInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(string(parts[0]))
	if err != nil || len(payload) == 0 || len(payload) > maxCursorPayloadSize {
		return CursorProgress{}, ErrCursorInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil || len(signature) != sha256.Size {
		return CursorProgress{}, ErrCursorInvalid
	}
	claims, err := decodeCursorClaims(payload)
	if err != nil || !validCursorPurpose(claims.Purpose) {
		return CursorProgress{}, ErrCursorInvalid
	}
	want := cursorMAC(codec.key, claims.Purpose, payload)
	if !hmac.Equal(signature, want) {
		return CursorProgress{}, ErrCursorInvalid
	}
	now := codec.now().UTC()
	if err := validateCursorClaims(claims, now); err != nil {
		return CursorProgress{}, err
	}
	if err := validateCursorIdentity(expected); err != nil {
		return CursorProgress{}, ErrCursorScopeMismatch
	}
	if !sameCursorIdentity(claims, expected) {
		return CursorProgress{}, ErrCursorScopeMismatch
	}
	progress := CursorProgress{ResultID: claims.ResultID, ResultDigest: claims.ResultDigest, LastKey: claims.LastKey, Position: claims.Position}
	if err := validateCursorProgress(progress); err != nil {
		return CursorProgress{}, ErrCursorInvalid
	}
	return progress, nil
}

func claimsFor(identity CursorIdentity, progress CursorProgress) cursorClaims {
	return cursorClaims{
		Purpose: identity.Purpose, Scope: identity.Scope, PrincipalID: identity.PrincipalID,
		AllowedBinding: identity.AllowedBindingID, RequestHash: identity.RequestHash,
		OrderVersion: identity.OrderVersion, BindingID: identity.BindingID,
		ResolverDigest: identity.ResolverDigest, MapID: identity.MapID, SessionID: identity.SessionID,
		DocumentEpoch: identity.DocumentEpoch, HMRRevision: identity.HMRRevision, AuthEpoch: identity.AuthEpoch,
		ResultID: progress.ResultID, ResultDigest: progress.ResultDigest,
		LastKey: progress.LastKey, Position: progress.Position,
	}
}

func validateCursorIdentity(identity CursorIdentity) error {
	if !validCursorPurpose(identity.Purpose) || identity.Scope.Validate() != nil ||
		!validOpaqueIdentity(string(identity.PrincipalID)) || !validOpaqueIdentity(string(identity.AllowedBindingID)) ||
		identity.OrderVersion == "" || len(identity.OrderVersion) > 64 || !utf8.ValidString(identity.OrderVersion) ||
		identity.DocumentEpoch < 0 || identity.HMRRevision < 0 || identity.AuthEpoch < 0 ||
		identity.DocumentEpoch > model.MaxSafeInteger || identity.HMRRevision > model.MaxSafeInteger || identity.AuthEpoch > model.MaxSafeInteger {
		return ErrCursorInvalid
	}
	if _, err := model.ParseHash(string(identity.RequestHash)); err != nil {
		return ErrCursorInvalid
	}
	if identity.BindingID != nil {
		if _, err := model.ParseUUID(string(*identity.BindingID)); err != nil {
			return ErrCursorInvalid
		}
	}
	if identity.SessionID != nil {
		if _, err := model.ParseUUID(string(*identity.SessionID)); err != nil {
			return ErrCursorInvalid
		}
	}
	for _, value := range []*model.Hash{identity.ResolverDigest, identity.MapID} {
		if value != nil {
			if _, err := model.ParseHash(string(*value)); err != nil {
				return ErrCursorInvalid
			}
		}
	}
	if identity.Purpose == "mapping" && (identity.BindingID == nil || identity.ResolverDigest == nil || identity.MapID == nil || identity.SessionID == nil) {
		return ErrCursorInvalid
	}
	return nil
}

func validateCursorProgress(progress CursorProgress) error {
	if progress.Position < 0 || progress.Position > model.MaxSafeInteger || !utf8.ValidString(progress.LastKey) || len(progress.LastKey) > 4096 {
		return ErrCursorInvalid
	}
	if progress.Position == 0 && progress.LastKey == "" {
		return fmt.Errorf("continuation cursor has no forward progress: %w", ErrCursorInvalid)
	}
	if progress.ResultID != nil {
		if _, err := model.ParseUUID(string(*progress.ResultID)); err != nil {
			return ErrCursorInvalid
		}
	}
	if progress.ResultDigest != nil {
		if _, err := model.ParseHash(string(*progress.ResultDigest)); err != nil {
			return ErrCursorInvalid
		}
	}
	if (progress.ResultID == nil) != (progress.ResultDigest == nil) {
		return ErrCursorInvalid
	}
	if (progress.ResultID != nil) && (progress.Position < 1 || progress.LastKey == "") {
		return ErrCursorInvalid
	}
	return nil
}

func validateCursorClaims(claims cursorClaims, now time.Time) error {
	if claims.Version != CursorVersion || !validCursorPurpose(claims.Purpose) || claims.Scope.Validate() != nil {
		return ErrCursorInvalid
	}
	identity := CursorIdentity{
		Purpose: claims.Purpose, Scope: claims.Scope, PrincipalID: claims.PrincipalID,
		AllowedBindingID: claims.AllowedBinding, RequestHash: claims.RequestHash,
		OrderVersion: claims.OrderVersion, BindingID: claims.BindingID,
		ResolverDigest: claims.ResolverDigest, MapID: claims.MapID, SessionID: claims.SessionID,
		DocumentEpoch: claims.DocumentEpoch, HMRRevision: claims.HMRRevision, AuthEpoch: claims.AuthEpoch,
	}
	if validateCursorIdentity(identity) != nil || claims.IssuedAtUnix < 0 || claims.ExpiresAtUnix <= claims.IssuedAtUnix ||
		claims.ExpiresAtUnix-claims.IssuedAtUnix > int64(MaxCursorTTL/time.Second) {
		return ErrCursorInvalid
	}
	if now.Unix() < claims.IssuedAtUnix {
		return ErrCursorInvalid
	}
	if now.Unix() >= claims.ExpiresAtUnix {
		return ErrCursorExpired
	}
	if err := validateCursorProgress(CursorProgress{ResultID: claims.ResultID, ResultDigest: claims.ResultDigest, LastKey: claims.LastKey, Position: claims.Position}); err != nil {
		return ErrCursorInvalid
	}
	return nil
}

func sameCursorIdentity(claims cursorClaims, expected CursorIdentity) bool {
	leftDigest, leftErr := claims.Scope.ComputeDigest()
	rightDigest, rightErr := expected.Scope.ComputeDigest()
	return leftErr == nil && rightErr == nil &&
		leftDigest == rightDigest && claims.Scope.CatalogIncarnation == expected.Scope.CatalogIncarnation &&
		claims.Purpose == expected.Purpose && claims.PrincipalID == expected.PrincipalID &&
		claims.AllowedBinding == expected.AllowedBindingID && claims.RequestHash == expected.RequestHash &&
		claims.OrderVersion == expected.OrderVersion && sameOptionalUUID(claims.BindingID, expected.BindingID) &&
		sameOptionalHash(claims.ResolverDigest, expected.ResolverDigest) && sameOptionalHash(claims.MapID, expected.MapID) &&
		sameOptionalUUID(claims.SessionID, expected.SessionID) && claims.DocumentEpoch == expected.DocumentEpoch &&
		claims.HMRRevision == expected.HMRRevision && claims.AuthEpoch == expected.AuthEpoch
}

func sameOptionalUUID(left, right *model.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameOptionalHash(left, right *model.Hash) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func validOpaqueIdentity(value string) bool {
	if len(value) == 0 || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func validCursorPurpose(value string) bool {
	switch value {
	case "pack", "list", "mapping", "rename-result", "rename-page":
		return true
	default:
		return false
	}
}

func cursorMAC(key []byte, purpose string, payload []byte) []byte {
	derive := hmac.New(sha256.New, key)
	_, _ = derive.Write([]byte("MCE4D\x00CURSOR-PURPOSE\x00" + purpose))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func canonicalCursorJSON(claims cursorClaims) ([]byte, error) {
	raw, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}
	value, err := contracts.DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	return model.CanonicalJSON1(value)
}

func decodeCursorClaims(raw []byte) (cursorClaims, error) {
	var claims cursorClaims
	value, err := contracts.DecodeJSON(raw)
	if err != nil {
		return claims, err
	}
	canonical, err := model.CanonicalJSON1(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return claims, ErrCursorInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return claims, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return claims, ErrCursorInvalid
	}
	return claims, nil
}
