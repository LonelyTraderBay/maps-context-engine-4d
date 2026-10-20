package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/ports"
)

type ProcessAdapter struct {
	descriptor Descriptor
	commandID  string
	timeout    time.Duration
	runner     ports.LanguageProviderProcess
}

func NewProcessAdapter(descriptor Descriptor, commandID string, timeout time.Duration, runner ports.LanguageProviderProcess) (*ProcessAdapter, error) {
	normalized, err := ValidateDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	if !validIdentifier(commandID) || timeout <= 0 || timeout > MaxProviderTimeout || runner == nil {
		return nil, fmt.Errorf("%w: command ID, runner and timeout up to %s are required", ErrInvalidProvider, MaxProviderTimeout)
	}
	return &ProcessAdapter{descriptor: normalized, commandID: commandID, timeout: timeout, runner: runner}, nil
}

func (adapter *ProcessAdapter) Descriptor() Descriptor {
	descriptor := adapter.descriptor
	descriptor.Languages = append([]string(nil), adapter.descriptor.Languages...)
	descriptor.Capabilities = append([]Capability(nil), adapter.descriptor.Capabilities...)
	return descriptor
}

func (adapter *ProcessAdapter) Analyze(ctx context.Context, request Request) (Result, error) {
	if adapter == nil || adapter.runner == nil || ctx == nil {
		return Result{}, fmt.Errorf("%w: adapter and context are required", ErrInvalidProviderInput)
	}
	envelope, sourceByPath, err := normalizeRequest(request, adapter.descriptor)
	if err != nil {
		return Result{}, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return Result{}, fmt.Errorf("%w: encode request: %v", ErrInvalidProviderInput, err)
	}
	if len(payload) > MaxProviderFrameBytes {
		return Result{}, fmt.Errorf("%w: encoded request exceeds %d bytes", ErrInvalidProviderInput, MaxProviderFrameBytes)
	}
	framedRequest, err := EncodeFrame(payload)
	if err != nil {
		return Result{}, err
	}
	processContext, cancel := context.WithTimeout(ctx, adapter.timeout)
	defer cancel()
	responseFrame, err := adapter.runner.Run(processContext, ports.LanguageProviderProcessRequest{
		CommandID: adapter.commandID, ExpectedRuntimeDigest: adapter.descriptor.Digest,
		FramedRequest: framedRequest, MaxResponseBytes: MaxProviderResponseBytes,
	})
	if err != nil {
		if errors.Is(processContext.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("%w: %v", ErrProviderTimeout, err)
		}
		if errors.Is(processContext.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return Result{}, fmt.Errorf("language provider canceled: %w", err)
		}
		return Result{}, fmt.Errorf("language provider command %q failed: %w", adapter.commandID, err)
	}
	if processContext.Err() != nil {
		if errors.Is(processContext.Err(), context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("%w: provider returned after its deadline", ErrProviderTimeout)
		}
		return Result{}, fmt.Errorf("language provider canceled: %w", processContext.Err())
	}
	body, err := DecodeFrame(responseFrame)
	if err != nil {
		return Result{}, err
	}
	if err := contracts.ValidateLanguageProviderMessage(body); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidProviderOutput, err)
	}
	var result Result
	if err := json.Unmarshal(body, &result); err != nil {
		return Result{}, fmt.Errorf("%w: decode response: %v", ErrInvalidProviderOutput, err)
	}
	if err := validateResult(result, request, adapter.descriptor, sourceByPath); err != nil {
		return Result{}, err
	}
	return result, nil
}
