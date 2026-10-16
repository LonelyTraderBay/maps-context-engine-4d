// Package ports declares small boundaries consumed by application services.
package ports

import (
	"context"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

// CaptureManifestArtifacts stores immutable manifest bytes outside scalar DB rows.
// Implementations verify the descriptor and raw artifact SHA before publishing.
type CaptureManifestArtifacts interface {
	Publish(ctx context.Context, ref model.StoredCaptureManifestRef, raw []byte) error
	Read(ctx context.Context, ref model.StoredCaptureManifestRef) ([]byte, error)
}
