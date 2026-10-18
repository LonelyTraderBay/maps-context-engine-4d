package cursor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

func cursorTestScope(t *testing.T) model.ScopeVector {
	t.Helper()
	vector := model.ScopeVector{
		Kind: "single", Members: []model.ScopeMember{{
			WorktreeID:     "11111111-1111-4111-8111-111111111111",
			ProfileID:      model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			GenerationID:   "22222222-2222-4222-8222-222222222222",
			SnapshotDigest: model.Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			PolicyDigest:   model.Hash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
			PolicyEpoch:    3, BindingRevision: 4, RecordEpoch: 5,
			IndexIncarnation:   "33333333-3333-4333-8333-333333333333",
			RecordsIncarnation: "44444444-4444-4444-8444-444444444444",
		}}, CatalogIncarnation: "55555555-5555-4555-8555-555555555555",
		UnavailableMembers: []model.UnavailableMember{},
	}
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	vector.VectorDigest = digest
	return vector
}

func cursorTestIdentity(t *testing.T) CursorIdentity {
	t.Helper()
	return CursorIdentity{
		Purpose: "list", Scope: cursorTestScope(t), PrincipalID: "principal:fixture",
		AllowedBindingID: "binding:fixture", RequestHash: model.Hash("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
		OrderVersion: "symbol-keyset-v1",
	}
}

func TestCursorCodecBindsScopeAuthorizationRequestAndProgress(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	codec, err := NewCursorCodec(key, 5*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	identity := cursorTestIdentity(t)
	token, err := codec.Issue(identity, CursorProgress{LastKey: "alpha\x00symbol-7", Position: 7})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := codec.Verify(token, identity)
	if err != nil || progress.LastKey != "alpha\x00symbol-7" || progress.Position != 7 {
		t.Fatalf("cursor roundtrip progress=%+v err=%v", progress, err)
	}

	changed := identity
	changed.PrincipalID = "principal:other"
	if _, err := codec.Verify(token, changed); !errors.Is(err, ErrCursorScopeMismatch) {
		t.Fatalf("cross-principal cursor error=%v", err)
	}
	changed = identity
	changed.AllowedBindingID = "binding:other"
	if _, err := codec.Verify(token, changed); !errors.Is(err, ErrCursorScopeMismatch) {
		t.Fatalf("cross-binding cursor error=%v", err)
	}
	changed = identity
	changed.RequestHash = model.Hash("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if _, err := codec.Verify(token, changed); !errors.Is(err, ErrCursorScopeMismatch) {
		t.Fatalf("changed request cursor error=%v", err)
	}
	changed = identity
	changed.Scope.Members[0].PolicyEpoch++
	changed.Scope.VectorDigest, err = changed.Scope.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(token, changed); !errors.Is(err, ErrCursorScopeMismatch) {
		t.Fatalf("changed policy epoch cursor error=%v", err)
	}
}

func TestCursorCodecRejectsTamperingExpiryRotationAndNoProgress(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	codec, err := NewCursorCodec(key, time.Second, clock)
	if err != nil {
		t.Fatal(err)
	}
	identity := cursorTestIdentity(t)
	if _, err := codec.Issue(identity, CursorProgress{}); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("empty progress error=%v", err)
	}
	token, err := codec.Issue(identity, CursorProgress{LastKey: "item-1", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	changed := token[:len(token)-1] + "A"
	if _, err := codec.Verify(changed, identity); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("tampered token error=%v", err)
	}
	now = now.Add(time.Second)
	if _, err := codec.Verify(token, identity); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("expired token error=%v", err)
	}
	now = now.Add(-time.Second)
	rotated, err := NewCursorCodec([]byte("abcdef0123456789abcdef0123456789"), time.Second, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Verify(token, identity); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("rotated-key token error=%v", err)
	}
}

func TestCursorCodecRejectsNonCanonicalSignedJSON(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	codec, err := NewCursorCodec(key, time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	identity := cursorTestIdentity(t)
	token, err := codec.Issue(identity, CursorProgress{LastKey: "item-1", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	payloadPart, _, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("issued cursor has invalid token shape: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		t.Fatalf("decode issued cursor payload: %v", err)
	}
	var nonCanonical bytes.Buffer
	if err := json.Indent(&nonCanonical, payload, "", "  "); err != nil {
		t.Fatalf("indent issued cursor payload: %v", err)
	}
	if bytes.Equal(nonCanonical.Bytes(), payload) {
		t.Fatal("test payload unexpectedly remained canonical")
	}
	forged := base64.RawURLEncoding.EncodeToString(nonCanonical.Bytes()) + "." +
		base64.RawURLEncoding.EncodeToString(cursorMAC(key, identity.Purpose, nonCanonical.Bytes()))
	if _, err := codec.Verify(forged, identity); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("validly signed non-canonical JSON was accepted: %v", err)
	}
}

func TestCursorPurposeRequiresMappingIdentityAndSeparateSignature(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	codec, err := NewCursorCodec(key, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := cursorTestIdentity(t)
	identity.Purpose = "mapping"
	if _, err := codec.Issue(identity, CursorProgress{LastKey: "anchor:target", Position: 1}); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("mapping cursor without resolver binding error=%v", err)
	}
	identity.BindingID = ptrUUID(model.UUID("66666666-6666-4666-8666-666666666666"))
	identity.ResolverDigest = ptrHash(model.Hash("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"))
	identity.MapID = ptrHash(model.Hash("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"))
	identity.SessionID = ptrUUID(model.UUID("77777777-7777-4777-8777-777777777777"))
	token, err := codec.Issue(identity, CursorProgress{LastKey: "anchor:target", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(token, identity); err != nil {
		t.Fatalf("valid mapping continuation error=%v", err)
	}

	changed := identity
	changed.HMRRevision++
	if _, err := codec.Verify(token, changed); !errors.Is(err, ErrCursorScopeMismatch) {
		t.Fatalf("changed HMR revision error=%v", err)
	}
}

func TestCursorKeyStoreCreatesReusesAndRotatesPrivateKey(t *testing.T) {
	home := t.TempDir()
	root, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx := context.Background()
	first, err := LoadOrCreateCursorKey(ctx, root, time.Second)
	if err != nil || len(first) != sha256.Size {
		t.Fatalf("create cursor key len=%d err=%v", len(first), err)
	}
	second, err := LoadOrCreateCursorKey(ctx, root, time.Second)
	if err != nil || !equalSecret(first, second) {
		t.Fatalf("cursor key was not stable: err=%v", err)
	}
	rotated, err := RotateCursorKey(ctx, root, time.Second)
	if err != nil || len(rotated) != sha256.Size || equalSecret(first, rotated) {
		t.Fatalf("cursor key rotation failed: err=%v", err)
	}

	path := filepath.Join(home, filepath.FromSlash(cursorKeyPath))
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCursorKey(ctx, root, time.Second); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("corrupt key should fail closed rather than rotate: %v", err)
	}
}

func ptrUUID(value model.UUID) *model.UUID { return &value }
func ptrHash(value model.Hash) *model.Hash { return &value }
