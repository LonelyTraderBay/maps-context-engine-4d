package indexer

import (
	"context"
	"errors"
	"fmt"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

var ErrGenerationRecoveryActive = errors.New("interrupted SQLite generation is still active")

// RecoveryResult reports the exact persisted generation after recovery. A
// completed publication is only observed; an abandoned BUILDING candidate is
// marked FAILED without deleting staged facts or moving the active pointer.
type RecoveryResult struct {
	Generation sqlite.GenerationRecord
	Active     bool
	Recovered  bool
}

// RecoverInterrupted resolves one persisted job after acquiring the same
// worktree OS writer lease used by Publish. The caller supplies the exact key
// saved with the job; timestamps and process IDs never decide ownership.
func RecoverInterrupted(ctx context.Context, store PublicationStore, key sqlite.GenerationKey) (RecoveryResult, error) {
	var result RecoveryResult
	if ctx == nil || store == nil {
		return result, fmt.Errorf("recover interrupted generation requires context and store")
	}
	err := store.WithGenerationWriter(ctx, func(writerCtx context.Context) error {
		resolution, err := store.ResolvePublication(writerCtx, key)
		if err != nil {
			return fmt.Errorf("resolve interrupted generation before recovery: %w", err)
		}
		if resolution.Generation.Key != key {
			return fmt.Errorf("resolved generation does not match requested owner key: %w", sqlite.ErrGenerationConflict)
		}
		if resolution.Generation.State == sqlite.GenerationBuilding {
			if resolution.Active {
				return fmt.Errorf("BUILDING generation is referenced by the active pointer: %w", ErrGenerationRecoveryActive)
			}
			if err := store.AbortGeneration(writerCtx, key, sqlite.GenerationFailed); err != nil {
				return fmt.Errorf("record interrupted generation failure: %w", err)
			}
			resolution, err = store.ResolvePublication(writerCtx, key)
			if err != nil {
				return fmt.Errorf("verify interrupted generation recovery: %w", err)
			}
			if resolution.Generation.Key != key {
				return fmt.Errorf("recovered generation does not match requested owner key: %w", sqlite.ErrGenerationConflict)
			}
			if resolution.Generation.State != sqlite.GenerationFailed || resolution.Active {
				return fmt.Errorf("recovered generation state or active pointer changed unexpectedly: %w", sqlite.ErrGenerationState)
			}
			result.Recovered = true
		} else {
			switch resolution.Generation.State {
			case sqlite.GenerationReady, sqlite.GenerationReadyPartial:
				// A previous publish may have committed before the process stopped.
			case sqlite.GenerationFailed, sqlite.GenerationCancelled, sqlite.GenerationRetired:
				if resolution.Active {
					return fmt.Errorf("terminal generation is referenced by the active pointer: %w", ErrGenerationRecoveryActive)
				}
			default:
				return fmt.Errorf("unsupported interrupted generation state %q: %w", resolution.Generation.State, sqlite.ErrGenerationState)
			}
		}
		result.Generation = resolution.Generation
		result.Active = resolution.Active
		return nil
	})
	if err != nil {
		return RecoveryResult{}, err
	}
	return result, nil
}

// cancelFailedGeneration finishes a bounded state-only write while Publish
// still owns the writer lease. WithoutCancel removes caller cancellation but
// preserves the lease token; the store still applies its configured lock,
// admission, and SQLite busy limits. Staged rows and source data are retained.
func cancelFailedGeneration(ctx context.Context, store PublicationStore, key sqlite.GenerationKey, terminal sqlite.GenerationState) error {
	if ctx == nil {
		return fmt.Errorf("generation recovery context is required")
	}
	if terminal != sqlite.GenerationFailed && terminal != sqlite.GenerationCancelled {
		return fmt.Errorf("invalid failed generation recovery state %q: %w", terminal, sqlite.ErrGenerationState)
	}
	cleanupCtx := ctx
	if terminal == sqlite.GenerationCancelled && ctx.Err() != nil {
		cleanupCtx = context.WithoutCancel(ctx)
	}
	return store.AbortGeneration(cleanupCtx, key, terminal)
}
