package providers

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
)

var frameSeparator = []byte("\r\n\r\n")

// EncodeFrame writes one strict Content-Length framed provider message.
func EncodeFrame(payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > MaxProviderFrameBytes {
		return nil, fmt.Errorf("%w: payload length %d is outside the protocol limit", ErrInvalidProviderInput, len(payload))
	}
	if err := contracts.ValidateLanguageProviderMessage(payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProviderInput, err)
	}
	header := []byte("Content-Length: " + strconv.Itoa(len(payload)))
	if len(header) > MaxProviderHeaderBytes {
		return nil, fmt.Errorf("%w: provider frame header exceeds %d bytes", ErrInvalidProviderInput, MaxProviderHeaderBytes)
	}
	frame := make([]byte, 0, len(header)+len(frameSeparator)+len(payload))
	frame = append(frame, header...)
	frame = append(frame, frameSeparator...)
	frame = append(frame, payload...)
	return frame, nil
}

// DecodeFrame parses exactly one complete frame and rejects extra headers,
// noncanonical lengths, partial payloads and trailing data.
func DecodeFrame(frame []byte) ([]byte, error) {
	if len(frame) > MaxProviderResponseBytes {
		return nil, fmt.Errorf("%w: framed response exceeds byte budget", ErrInvalidProviderOutput)
	}
	separator := bytes.Index(frame, frameSeparator)
	if separator < 0 {
		if len(frame) > MaxProviderHeaderBytes {
			return nil, fmt.Errorf("%w: provider frame header exceeds %d bytes", ErrInvalidProviderOutput, MaxProviderHeaderBytes)
		}
		return nil, fmt.Errorf("%w: incomplete provider frame header", ErrInvalidProviderOutput)
	}
	if separator > MaxProviderHeaderBytes {
		return nil, fmt.Errorf("%w: provider frame header exceeds %d bytes", ErrInvalidProviderOutput, MaxProviderHeaderBytes)
	}
	header := frame[:separator]
	const prefix = "Content-Length: "
	if !bytes.HasPrefix(header, []byte(prefix)) {
		return nil, fmt.Errorf("%w: expected one Content-Length header", ErrInvalidProviderOutput)
	}
	digits := string(header[len(prefix):])
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return nil, fmt.Errorf("%w: noncanonical Content-Length", ErrInvalidProviderOutput)
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return nil, fmt.Errorf("%w: Content-Length must be ASCII decimal", ErrInvalidProviderOutput)
		}
	}
	length, err := strconv.ParseUint(digits, 10, 32)
	if err != nil || length == 0 || length > MaxProviderFrameBytes {
		return nil, fmt.Errorf("%w: Content-Length outside protocol limit", ErrInvalidProviderOutput)
	}
	bodyStart := separator + len(frameSeparator)
	bodyEnd := bodyStart + int(length)
	if bodyEnd < bodyStart || bodyEnd != len(frame) {
		return nil, fmt.Errorf("%w: incomplete payload or trailing frame data", ErrInvalidProviderOutput)
	}
	body := frame[bodyStart:bodyEnd]
	if err := contracts.ValidateLanguageProviderMessage(body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProviderOutput, err)
	}
	return append([]byte(nil), body...), nil
}
