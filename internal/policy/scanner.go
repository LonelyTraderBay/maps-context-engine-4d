package policy

import (
	"bytes"
	"regexp"
)

const (
	SecretRulesVersion = "mce4d-secret-v1"
	secretMarkerPrefix = "MCE4D_FAKE_SECRET_"
)

var (
	pemPrivateKey = regexp.MustCompile(`(?m)^-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----\r?$`)
	literalSecret = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|private[_-]?key|password|passwd|mnemonic|seed[_-]?phrase)\b["']?\s*(?::=|=|:)\s*["'][^"'\\\r\n]+["']`)
)

func scanSecret(value []byte) (string, error) {
	if pemPrivateKey.Match(value) {
		return "pem_private_key", ErrSecretDetected
	}
	if literalSecret.Match(value) {
		return "literal_credential_assignment", ErrSecretDetected
	}
	if hasSyntheticMarker(value) {
		return "synthetic_test_marker", ErrSecretDetected
	}
	return "", nil
}

func hasSyntheticMarker(value []byte) bool {
	for offset := 0; offset < len(value); {
		index := bytes.Index(value[offset:], []byte(secretMarkerPrefix))
		if index < 0 {
			return false
		}
		start := offset + index + len(secretMarkerPrefix)
		end := start + 32
		if end <= len(value) {
			valid := true
			for _, character := range value[start:end] {
				if !(character >= '0' && character <= '9' || character >= 'A' && character <= 'F') {
					valid = false
					break
				}
			}
			if valid && (end == len(value) || !isHex(value[end])) {
				return true
			}
		}
		offset = start
	}
	return false
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'A' && value <= 'F' || value >= 'a' && value <= 'f'
}
