// Package ports declares small boundaries consumed by application services.
package ports

import (
	"context"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

// LanguageProviderProcessRequest is a framed request to one host-registered
// provider command. CommandID resolves to a sealed executable/runtime digest,
// static arguments and environment; callers cannot pass project paths, argv,
// plugin names or scripts through this port.
type LanguageProviderProcessRequest struct {
	CommandID             string
	ExpectedRuntimeDigest model.Hash
	FramedRequest         []byte
	MaxResponseBytes      int
}

// LanguageProviderProcess runs one approved provider command. Implementations
// must match the executable digest, honor ctx cancellation, enforce the
// response byte cap while reading, invoke the exact registered executable
// without a shell, deny network access, and avoid inheriting ambient environment
// variables. A command entry must not resolve through target repository config.
type LanguageProviderProcess interface {
	Run(ctx context.Context, request LanguageProviderProcessRequest) ([]byte, error)
}
