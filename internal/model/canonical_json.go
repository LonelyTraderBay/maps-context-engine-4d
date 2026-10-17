package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CanonicalJSON1 encodes the safe-integer JSON envelope used by manifest tools.
// It is separate from MCE-C14N1, which encodes typed values for content hashes.
func CanonicalJSON1(value any) ([]byte, error) {
	return appendJSON1(nil, value, 0)
}

// WriteCaptureManifestJSON1 streams the normalized physical manifest form.
// The caller can hash or persist bytes without retaining the entire JSON output.
func WriteCaptureManifestJSON1(dst io.Writer, manifest CaptureManifest) error {
	if dst == nil {
		return fmt.Errorf("%w: nil manifest writer", ErrInvalidCanonicalValue)
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("%w: invalid capture manifest: %v", ErrInvalidCanonicalValue, err)
	}
	files := append([]ManifestFile(nil), manifest.Files...)
	sort.Slice(files, func(i, j int) bool {
		return bytes.Compare([]byte(files[i].Path), []byte(files[j].Path)) < 0
	})
	dependencies := append([]Hash(nil), manifest.DependencyInputDigests...)
	sort.Slice(dependencies, func(i, j int) bool {
		return bytes.Compare([]byte(dependencies[i]), []byte(dependencies[j])) < 0
	})
	stream := CaptureManifestSliceSources(manifest)
	stream.Files = &sliceManifestFiles{values: files}
	stream.Dependencies = &sliceManifestHashes{values: dependencies}
	_, err := WriteCaptureManifestJSON1Stream(dst, stream)
	return err
}

func writeJSON1Value(dst io.Writer, value any) error {
	encoded, err := CanonicalJSON1(value)
	if err != nil {
		return err
	}
	return writeCanonicalBytes(dst, encoded)
}

func writeCanonicalBytes(dst io.Writer, data []byte) error {
	written, err := dst.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func appendJSON1(dst []byte, value any, depth int) ([]byte, error) {
	if depth > maxCanonicalDepth {
		return nil, fmt.Errorf("%w: maximum JSON depth exceeded", ErrInvalidCanonicalValue)
	}
	switch typed := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		if typed {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case string:
		return appendJSONString(dst, typed)
	case UUID:
		return appendJSONString(dst, string(typed))
	case Hash:
		return appendJSONString(dst, string(typed))
	case Identifier:
		return appendJSONString(dst, string(typed))
	case AdmissionOwnedPath:
		return appendJSONString(dst, string(typed))
	case json.Number:
		return appendJSON1Integer(dst, typed)
	case int:
		return appendJSON1Integer(dst, json.Number(strconv.FormatInt(int64(typed), 10)))
	case int8:
		return appendJSON1Integer(dst, json.Number(strconv.FormatInt(int64(typed), 10)))
	case int16:
		return appendJSON1Integer(dst, json.Number(strconv.FormatInt(int64(typed), 10)))
	case int32:
		return appendJSON1Integer(dst, json.Number(strconv.FormatInt(int64(typed), 10)))
	case int64:
		return appendJSON1Integer(dst, json.Number(strconv.FormatInt(typed, 10)))
	case uint:
		return appendJSON1Integer(dst, json.Number(strconv.FormatUint(uint64(typed), 10)))
	case uint8:
		return appendJSON1Integer(dst, json.Number(strconv.FormatUint(uint64(typed), 10)))
	case uint16:
		return appendJSON1Integer(dst, json.Number(strconv.FormatUint(uint64(typed), 10)))
	case uint32:
		return appendJSON1Integer(dst, json.Number(strconv.FormatUint(uint64(typed), 10)))
	case uint64:
		return appendJSON1Integer(dst, json.Number(strconv.FormatUint(typed, 10)))
	case []any:
		if depth == maxCanonicalDepth {
			return nil, fmt.Errorf("%w: maximum JSON depth exceeded", ErrInvalidCanonicalValue)
		}
		dst = append(dst, '[')
		for index, item := range typed {
			if index > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendJSON1(dst, item, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		if len(typed) > maxObjectKeys {
			return nil, fmt.Errorf("%w: too many JSON object keys", ErrInvalidCanonicalValue)
		}
		if depth == maxCanonicalDepth {
			return nil, fmt.Errorf("%w: maximum JSON depth exceeded", ErrInvalidCanonicalValue)
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return nil, fmt.Errorf("%w: object key is not UTF-8", ErrInvalidCanonicalValue)
			}
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return bytes.Compare([]byte(keys[i]), []byte(keys[j])) < 0 })
		dst = append(dst, '{')
		for index, key := range keys {
			if index > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendJSONString(dst, key)
			if err != nil {
				return nil, err
			}
			dst = append(dst, ':')
			dst, err = appendJSON1(dst, typed[key], depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	default:
		return nil, fmt.Errorf("%w: unsupported JSON type %T", ErrInvalidCanonicalValue, value)
	}
}

func appendJSONString(dst []byte, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%w: string is not valid UTF-8", ErrInvalidCanonicalValue)
	}
	dst = append(dst, '"')
	for _, char := range value {
		switch char {
		case '"', '\\':
			dst = append(dst, '\\', byte(char))
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\u2028':
			dst = append(dst, "\\u2028"...)
		case '\u2029':
			dst = append(dst, "\\u2029"...)
		default:
			if char < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0')
				const lowerHex = "0123456789abcdef"
				dst = append(dst, lowerHex[byte(char)>>4], lowerHex[byte(char)&0x0f])
			} else {
				dst = utf8.AppendRune(dst, char)
			}
		}
	}
	return append(dst, '"'), nil
}

func appendJSON1Integer(dst []byte, number json.Number) ([]byte, error) {
	lexeme := string(number)
	if lexeme == "" || strings.ContainsAny(lexeme, ".eE+") {
		return nil, fmt.Errorf("%w: fraction/exponent is not allowed", ErrInvalidCanonicalValue)
	}
	parsed, err := strconv.ParseInt(lexeme, 10, 64)
	if err != nil || parsed > MaxSafeInteger || parsed < -MaxSafeInteger {
		return nil, fmt.Errorf("%w: integer outside safe range", ErrInvalidCanonicalValue)
	}
	canonical := strconv.FormatInt(parsed, 10)
	if lexeme != canonical && lexeme != "-0" {
		return nil, fmt.Errorf("%w: integer is not minimally encoded", ErrInvalidCanonicalValue)
	}
	return append(dst, canonical...), nil
}
