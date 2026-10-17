package indexer

import (
	"context"
	"errors"
	"fmt"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

// PublicationStore exposes only the generation lifecycle needed by the
// pipeline. One worktree writer lease spans staging, verification, and publish;
// every database batch still uses a short admitted SQL transaction.
type PublicationStore interface {
	WithGenerationWriter(context.Context, func(context.Context) error) error
	CreateGeneration(context.Context, sqlite.GenerationSpec) (sqlite.GenerationRecord, error)
	PublishGeneration(context.Context, sqlite.PublishRequest) (sqlite.PublishResult, error)
	AbortGeneration(context.Context, sqlite.GenerationKey, sqlite.GenerationState) error
	ResolvePublication(context.Context, sqlite.GenerationKey) (sqlite.PublicationResolution, error)
}

// GenerationStager writes finite batches and independently validates the
// staged facts and capture manifest before the active pointer can move.
type GenerationStager interface {
	Stage(context.Context, sqlite.GenerationRecord) error
	Verify(context.Context, sqlite.GenerationRecord) error
}

// Publish creates or resumes the exact generation while holding the worktree
// writer lease, stages finite batches, validates the captured inputs, and then
// delegates the READY transition and profile-pointer swap to one SQLite
// transaction. Stage must be idempotent for a repeated generation ID because a
// process can stop between batches.
func Publish(
	ctx context.Context,
	store PublicationStore,
	stager GenerationStager,
	spec sqlite.GenerationSpec,
	state sqlite.GenerationState,
) (sqlite.PublishResult, error) {
	var empty sqlite.PublishResult
	var result sqlite.PublishResult
	if ctx == nil || store == nil || stager == nil {
		return empty, fmt.Errorf("publish requires context, generation store, and stager")
	}
	if state != sqlite.GenerationReady && state != sqlite.GenerationReadyPartial {
		return empty, fmt.Errorf("publish target must be READY or READY_PARTIAL")
	}
	err := store.WithGenerationWriter(ctx, func(writerCtx context.Context) error {
		generation, err := store.CreateGeneration(writerCtx, spec)
		if err != nil {
			return fmt.Errorf("create or resolve staged generation: %w", err)
		}
		if generation.Key != spec.Key || generation.SnapshotDigest != spec.SnapshotDigest || generation.ManifestRef == nil || *generation.ManifestRef != spec.ManifestRef {
			return sqlite.ErrGenerationConflict
		}
		if generation.State == state {
			resolved, err := store.ResolvePublication(writerCtx, spec.Key)
			if err != nil {
				return fmt.Errorf("resolve prior generation publication: %w", err)
			}
			if resolved.Generation.State != state {
				return sqlite.ErrGenerationConflict
			}
			result = sqlite.PublishResult{Generation: resolved.Generation, Activated: resolved.Active, AlreadyPublished: true}
			return nil
		}
		if generation.State != sqlite.GenerationBuilding {
			return fmt.Errorf("generation %s cannot be resumed from %s: %w", generation.Key.GenerationID, generation.State, sqlite.ErrGenerationState)
		}

		if err := stager.Stage(writerCtx, generation); err != nil {
			return failGeneration(writerCtx, store, spec.Key, err)
		}
		if err := stager.Verify(writerCtx, generation); err != nil {
			return failGeneration(writerCtx, store, spec.Key, err)
		}
		result, err = store.PublishGeneration(writerCtx, sqlite.PublishRequest{Spec: spec, State: state})
		if err != nil {
			return fmt.Errorf("publish generation atomically: %w", err)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func failGeneration(ctx context.Context, store PublicationStore, key sqlite.GenerationKey, cause error) error {
	if ctx.Err() != nil {
		// A canceled context cannot safely start a cleanup write. T009 owns
		// bounded recovery of this still-BUILDING candidate; active stays intact.
		return cause
	}
	state := sqlite.GenerationFailed
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		state = sqlite.GenerationCancelled
	}
	if err := store.AbortGeneration(ctx, key, state); err != nil {
		return errors.Join(cause, fmt.Errorf("record failed generation state: %w", err))
	}
	return cause
}
