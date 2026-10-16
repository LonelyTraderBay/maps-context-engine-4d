package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxCanonicalDepth = 32
	maxObjectKeys     = 4096
)

var ErrInvalidCanonicalValue = errors.New("value cannot be encoded as MCE-C14N1")

// CanonicalEncode applies the scalar/container encoding from S43 CT-04.
// Recipe-specific array ordering remains the caller's responsibility.
func CanonicalEncode(value any) ([]byte, error) {
	return appendValue(nil, value, 0)
}

func appendValue(dst []byte, value any, depth int) ([]byte, error) {
	if depth > maxCanonicalDepth {
		return nil, fmt.Errorf("%w: maximum depth exceeded", ErrInvalidCanonicalValue)
	}
	switch typed := value.(type) {
	case nil:
		return append(dst, 'n'), nil
	case bool:
		if typed {
			return append(dst, 't'), nil
		}
		return append(dst, 'f'), nil
	case string:
		return appendString(dst, typed)
	case UUID:
		return appendString(dst, string(typed))
	case Hash:
		return appendString(dst, string(typed))
	case Identifier:
		return appendString(dst, string(typed))
	case AdmissionOwnedPath:
		return appendString(dst, string(typed))
	case json.Number:
		return appendJSONInteger(dst, typed)
	case int:
		return appendInteger(dst, int64(typed))
	case int8:
		return appendInteger(dst, int64(typed))
	case int16:
		return appendInteger(dst, int64(typed))
	case int32:
		return appendInteger(dst, int64(typed))
	case int64:
		return appendInteger(dst, typed)
	case uint:
		return appendUnsigned(dst, uint64(typed))
	case uint8:
		return appendUnsigned(dst, uint64(typed))
	case uint16:
		return appendUnsigned(dst, uint64(typed))
	case uint32:
		return appendUnsigned(dst, uint64(typed))
	case uint64:
		return appendUnsigned(dst, typed)
	case float32, float64:
		return nil, fmt.Errorf("%w: floating point values are not identities", ErrInvalidCanonicalValue)
	case []any:
		if depth == maxCanonicalDepth {
			return nil, fmt.Errorf("%w: maximum depth exceeded", ErrInvalidCanonicalValue)
		}
		dst = append(dst, 'a')
		dst = strconv.AppendInt(dst, int64(len(typed)), 10)
		dst = append(dst, ':')
		for _, item := range typed {
			var err error
			dst, err = appendValue(dst, item, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return dst, nil
	case map[string]any:
		if len(typed) > maxObjectKeys {
			return nil, fmt.Errorf("%w: object has more than %d keys", ErrInvalidCanonicalValue, maxObjectKeys)
		}
		if depth == maxCanonicalDepth {
			return nil, fmt.Errorf("%w: maximum depth exceeded", ErrInvalidCanonicalValue)
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return nil, fmt.Errorf("%w: object key is not UTF-8", ErrInvalidCanonicalValue)
			}
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return bytes.Compare([]byte(keys[i]), []byte(keys[j])) < 0 })
		dst = append(dst, 'o')
		dst = strconv.AppendInt(dst, int64(len(keys)), 10)
		dst = append(dst, ':')
		for _, key := range keys {
			var err error
			dst, err = appendString(dst, key)
			if err != nil {
				return nil, err
			}
			dst, err = appendValue(dst, typed[key], depth+1)
			if err != nil {
				return nil, err
			}
		}
		return dst, nil
	default:
		return nil, fmt.Errorf("%w: unsupported type %T", ErrInvalidCanonicalValue, value)
	}
}

func appendString(dst []byte, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%w: string is not valid UTF-8", ErrInvalidCanonicalValue)
	}
	dst = append(dst, 's')
	dst = strconv.AppendInt(dst, int64(len(value)), 10)
	dst = append(dst, ':')
	return append(dst, value...), nil
}

func appendInteger(dst []byte, value int64) ([]byte, error) {
	if value > MaxSafeInteger || value < -MaxSafeInteger {
		return nil, fmt.Errorf("%w: integer outside safe range", ErrInvalidCanonicalValue)
	}
	dst = append(dst, 'i')
	dst = strconv.AppendInt(dst, value, 10)
	return append(dst, ';'), nil
}

func appendUnsigned(dst []byte, value uint64) ([]byte, error) {
	if value > uint64(MaxSafeInteger) {
		return nil, fmt.Errorf("%w: integer outside safe range", ErrInvalidCanonicalValue)
	}
	dst = append(dst, 'i')
	dst = strconv.AppendUint(dst, value, 10)
	return append(dst, ';'), nil
}

func appendJSONInteger(dst []byte, value json.Number) ([]byte, error) {
	lexeme := string(value)
	if lexeme == "" || strings.ContainsAny(lexeme, ".eE+") {
		return nil, fmt.Errorf("%w: identity number must be a base-10 integer token", ErrInvalidCanonicalValue)
	}
	parsed, err := strconv.ParseInt(lexeme, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: integer token outside range", ErrInvalidCanonicalValue)
	}
	canonical := strconv.FormatInt(parsed, 10)
	if lexeme != canonical && lexeme != "-0" {
		return nil, fmt.Errorf("%w: integer token is not minimally encoded", ErrInvalidCanonicalValue)
	}
	return appendInteger(dst, parsed)
}

// HashValue implements H(domain,value) from S43 CT-04.
func HashValue(domain string, value any) (Hash, error) {
	if !utf8.ValidString(domain) {
		return "", fmt.Errorf("%w: domain is not valid UTF-8", ErrInvalidCanonicalValue)
	}
	domainBytes, err := CanonicalEncode(domain)
	if err != nil {
		return "", err
	}
	valueBytes, err := CanonicalEncode(value)
	if err != nil {
		return "", err
	}
	preimage := make([]byte, 0, len("MCE4D\x00C14N1\x00")+len(domainBytes)+len(valueBytes))
	preimage = append(preimage, []byte("MCE4D\x00C14N1\x00")...)
	preimage = append(preimage, domainBytes...)
	preimage = append(preimage, valueBytes...)
	digest := sha256.Sum256(preimage)
	return Hash(hex.EncodeToString(digest[:])), nil
}
