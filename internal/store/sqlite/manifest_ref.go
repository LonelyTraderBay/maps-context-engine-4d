package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	privateValidatorOnce sync.Once
	privateValidator     *contracts.Validator
	privateValidatorErr  error
)

// EncodeStoredCaptureManifestRef validates a private S43 descriptor and emits
// its canonical physical JSON representation.
func EncodeStoredCaptureManifestRef(ref model.StoredCaptureManifestRef) ([]byte, error) {
	value := map[string]any{
		"artifact_sha256": ref.ArtifactSHA256,
		"byte_length":     ref.ByteLength,
		"format_version":  ref.FormatVersion,
		"kind":            ref.Kind,
		"path":            ref.Path,
		"snapshot_digest": ref.SnapshotDigest,
	}
	raw, err := model.CanonicalJSON1(value)
	if err != nil {
		return nil, fmt.Errorf("encode capture manifest reference: %w", err)
	}
	if err := validatePrivate("StoredCaptureManifestRef", raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// DecodeStoredCaptureManifestRef rejects malformed, duplicate-key, unknown,
// semantically invalid, and noncanonical descriptor JSON.
func DecodeStoredCaptureManifestRef(raw []byte) (model.StoredCaptureManifestRef, error) {
	var ref model.StoredCaptureManifestRef
	if err := validatePrivate("StoredCaptureManifestRef", raw); err != nil {
		return ref, err
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		return model.StoredCaptureManifestRef{}, fmt.Errorf("decode capture manifest reference: %w", err)
	}
	canonical, err := EncodeStoredCaptureManifestRef(ref)
	if err != nil {
		return model.StoredCaptureManifestRef{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return model.StoredCaptureManifestRef{}, fmt.Errorf("capture manifest reference is not canonical MCE-MANIFEST-JSON1")
	}
	return ref, nil
}

// putCaptureManifestRef attaches a validated descriptor to a BUILDING
// generation. It remains private until T010 wires lifecycle and quota admission.
// The caller still owns the artifact-before-SQL publication order.
func (store *Store) putCaptureManifestRef(ctx context.Context, generationID string, ref model.StoredCaptureManifestRef) error {
	if store == nil || store.writer == nil || ctx == nil || generationID == "" {
		return fmt.Errorf("invalid capture manifest write request: %w", ErrInvalidIndex)
	}
	raw, err := EncodeStoredCaptureManifestRef(ref)
	if err != nil {
		return err
	}
	peakGrowth, ok := addNonnegative(int64(len(raw)), manifestWriteOverhead)
	if !ok || peakGrowth > model.MaxSafeInteger {
		return ErrAdmissionReservationOverflow
	}
	return store.withWriteTx(ctx, peakGrowth, func(tx *writeTx) error {
		result, err := tx.ExecContext(ctx, `UPDATE generations
			SET manifest_json=?
			WHERE generation_id=? AND state='BUILDING'
			  AND (snapshot_digest IS NULL OR snapshot_digest=?)`, string(raw), generationID, ref.SnapshotDigest)
		if err != nil {
			return fmt.Errorf("store capture manifest reference: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read capture manifest update count: %w", err)
		}
		if rows != 1 {
			return fmt.Errorf("generation %q is absent, not BUILDING, or has a different snapshot digest", generationID)
		}
		return nil
	})
}

// CaptureManifestRef reads and revalidates a descriptor at the SQL boundary.
// READY rows must bind the descriptor to their immutable snapshot digest.
func (store *Store) CaptureManifestRef(ctx context.Context, generationID string) (model.StoredCaptureManifestRef, error) {
	if store == nil || store.readers == nil || ctx == nil || generationID == "" {
		return model.StoredCaptureManifestRef{}, fmt.Errorf("invalid capture manifest read request: %w", ErrInvalidIndex)
	}
	var raw sql.NullString
	var snapshotDigest sql.NullString
	err := store.readers.QueryRowContext(ctx, `SELECT manifest_json,snapshot_digest FROM generations WHERE generation_id=?`, generationID).Scan(&raw, &snapshotDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return model.StoredCaptureManifestRef{}, fmt.Errorf("generation %q: %w", generationID, ErrManifestUnavailable)
	}
	if err != nil {
		return model.StoredCaptureManifestRef{}, fmt.Errorf("read capture manifest reference: %w", err)
	}
	if !raw.Valid {
		return model.StoredCaptureManifestRef{}, fmt.Errorf("generation %q: %w", generationID, ErrManifestUnavailable)
	}
	ref, err := DecodeStoredCaptureManifestRef([]byte(raw.String))
	if err != nil {
		return model.StoredCaptureManifestRef{}, err
	}
	if snapshotDigest.Valid && snapshotDigest.String != string(ref.SnapshotDigest) {
		return model.StoredCaptureManifestRef{}, fmt.Errorf("generation %q snapshot digest differs from its capture manifest reference: %w", generationID, ErrInvalidIndex)
	}
	return ref, nil
}

func validatePrivate(name string, raw []byte) error {
	privateValidatorOnce.Do(func() {
		privateValidator, privateValidatorErr = contracts.NewValidator()
	})
	if privateValidatorErr != nil {
		return fmt.Errorf("initialize private contract validator: %w", privateValidatorErr)
	}
	if err := privateValidator.ValidatePrivateRecord(name, raw); err != nil {
		return fmt.Errorf("invalid private %s record: %w", name, err)
	}
	return nil
}
