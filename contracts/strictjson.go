package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

const (
	MaxJSONBytes  = 16 << 20
	maxJSONDepth  = 32
	maxObjectKeys = 4096
)

var ErrInvalidJSON = errors.New("invalid contract JSON")

// DecodeJSON rejects duplicate members, trailing values, malformed UTF-8,
// lone surrogate escapes, excessive nesting, and oversized objects.
func DecodeJSON(raw []byte) (any, error) {
	if len(raw) > MaxJSONBytes {
		return nil, fmt.Errorf("%w: input exceeds %d bytes", ErrInvalidJSON, MaxJSONBytes)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: input is not valid UTF-8", ErrInvalidJSON)
	}
	if err := validateSurrogateEscapes(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%w: trailing JSON value", ErrInvalidJSON)
		}
		return nil, fmt.Errorf("%w: trailing input: %v", ErrInvalidJSON, err)
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	if depth >= maxJSONDepth {
		return nil, fmt.Errorf("maximum JSON depth %d exceeded", maxJSONDepth)
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object member name is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate object member %q", key)
			}
			if len(object) >= maxObjectKeys {
				return nil, fmt.Errorf("object exceeds %d members", maxObjectKeys)
			}
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func validateSurrogateEscapes(raw []byte) error {
	for i := 0; i < len(raw); {
		if raw[i] != '"' {
			i++
			continue
		}
		i++
		for i < len(raw) && raw[i] != '"' {
			if raw[i] != '\\' {
				i++
				continue
			}
			if i+1 >= len(raw) {
				break
			}
			if raw[i+1] != 'u' {
				i += 2
				continue
			}
			if i+6 > len(raw) {
				break
			}
			code, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
			if err != nil {
				i += 6
				continue
			}
			switch {
			case code >= 0xd800 && code <= 0xdbff:
				if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
					return fmt.Errorf("%w: unpaired high surrogate escape", ErrInvalidJSON)
				}
				low, lowErr := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16)
				if lowErr != nil || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("%w: high surrogate is not followed by a low surrogate", ErrInvalidJSON)
				}
				i += 12
			case code >= 0xdc00 && code <= 0xdfff:
				return fmt.Errorf("%w: unpaired low surrogate escape", ErrInvalidJSON)
			default:
				i += 6
			}
		}
		i++
	}
	return nil
}
