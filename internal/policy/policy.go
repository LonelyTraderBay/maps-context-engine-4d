package policy

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
)

var (
	ErrSecretDetected      = errors.New("secret-like content blocked")
	ErrContentTooLarge     = errors.New("content exceeds policy limit")
	ErrBinaryContent       = errors.New("binary content is not allowed")
	ErrInvalidUTF8         = errors.New("content is not valid UTF-8")
	ErrUnsupportedTextType = errors.New("file type is outside the default text allowlist")
)

const MaxFileBytes int64 = 2 * 1024 * 1024

var builtinDenyGlobs = []string{
	"**/.git/**",
	"**/.env",
	"**/.env.*",
	"**/*.pem",
	"**/*.key",
	"**/*.p12",
	"**/*.pfx",
	"**/id_rsa",
	"**/id_dsa",
	"**/id_ecdsa",
	"**/id_ed25519",
	"**/.aws/**",
	"**/.ssh/**",
	"**/.gnupg/**",
	"**/.netrc",
	"**/.npmrc",
	"**/.pypirc",
	"**/credentials.json",
	"**/secrets.json",
	"**/wallet.dat",
	"**/keystore/**",
	"**/node_modules/**",
	"**/vendor/**",
	"**/dist/**",
	"**/build/**",
	"**/coverage/**",
	"**/*.dump",
	"**/*.sql.gz",
	"**/*.log",
}

// BuiltinDenyGlobs returns the immutable baseline copied into default policies.
func BuiltinDenyGlobs() []string {
	return append([]string(nil), builtinDenyGlobs...)
}

type Decision struct {
	Denied bool
	Reason string
}

type Policy struct {
	builtins []Pattern
	user     []Pattern
}

func New(deniedGlobs []string, mode paths.CaseMode) (*Policy, error) {
	result := &Policy{builtins: make([]Pattern, 0, len(builtinDenyGlobs)), user: make([]Pattern, 0, len(deniedGlobs))}
	for _, source := range builtinDenyGlobs {
		pattern, err := compileBuiltin(source)
		if err != nil {
			return nil, err
		}
		result.builtins = append(result.builtins, pattern)
	}
	if len(deniedGlobs) != 0 && mode == paths.CaseUnknown {
		return nil, ErrCaseSemanticsUnknown
	}
	for _, source := range deniedGlobs {
		pattern, err := Compile(source, mode)
		if err != nil {
			return nil, err
		}
		result.user = append(result.user, pattern)
	}
	return result, nil
}

func (p *Policy) CheckPath(value string, isDirectory bool) (Decision, error) {
	if p == nil {
		return Decision{}, errors.New("policy is required")
	}
	if err := paths.ValidateRelative(value); err != nil {
		return Decision{}, err
	}
	builtInReason := "built_in_deny"
	userReason := "user_deny"
	if isDirectory {
		builtInReason = "built_in_directory_deny"
		userReason = "user_directory_deny"
	}
	for _, pattern := range p.builtins {
		matched, err := pattern.Match(value)
		if err != nil {
			return Decision{}, err
		}
		if matched {
			return Decision{Denied: true, Reason: builtInReason}, nil
		}
	}
	for _, pattern := range p.user {
		matched, err := pattern.Match(value)
		if err != nil {
			return Decision{}, err
		}
		if matched {
			return Decision{Denied: true, Reason: userReason}, nil
		}
	}
	return Decision{}, nil
}

func (p *Policy) AllowsText(value string) bool {
	if paths.ValidateRelative(value) != nil {
		return false
	}
	base := value
	if slash := strings.LastIndexByte(base, '/'); slash >= 0 {
		base = base[slash+1:]
	}
	name := asciiLower(base)
	switch name {
	case "dockerfile", "makefile", "license", "notice":
		return true
	}
	extension := name
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		extension = name[dot:]
	}
	switch extension {
	case ".go", ".mod", ".sum", ".work", ".md", ".txt", ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".sql", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs", ".css", ".html", ".xml", ".proto", ".tf":
		return true
	default:
		return false
	}
}

func (p *Policy) ScanContent(value []byte) (string, error) {
	if int64(len(value)) > MaxFileBytes {
		return "", ErrContentTooLarge
	}
	if bytes.IndexByte(value, 0) >= 0 {
		return "", ErrBinaryContent
	}
	if !utf8.Valid(value) {
		return "", ErrInvalidUTF8
	}
	return scanSecret(value)
}

func asciiLower(value string) string {
	bytes := []byte(value)
	for i, b := range bytes {
		if b >= 'A' && b <= 'Z' {
			bytes[i] = b + ('a' - 'A')
		}
	}
	return string(bytes)
}
