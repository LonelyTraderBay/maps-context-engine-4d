package contracts

import (
	"strings"
	"testing"
)

const providerRequestFixture = `{"schema_version":1,"message_type":"analyze_request","request_id":"123e4567-e89b-12d3-a456-426614174000","language":"typescript","profile":{"schema_id":"typescript-profile","schema_version":1,"profile_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","config":{"analysis_mode":"syntax"}},"files":[{"path":"src/main.ts","content_base64":"YQ==","content_sha256":"ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"}],"execution_policy":{"target_code":"never_execute","project_scripts":"never_execute","project_plugins":"never_execute","notebook_cells":"never_execute","network":false}}`

const providerResponseFixture = `{"schema_version":1,"message_type":"analyze_response","request_id":"123e4567-e89b-12d3-a456-426614174000","provider":{"id":"typescript-v6","version":"1.0.0","license":"Apache-2.0","digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","languages":["typescript"],"capabilities":[{"id":"symbols.syntax","version":"1.0.0"}]},"symbols":[],"relations":[],"diagnostics":[]}`

func TestValidateLanguageProviderMessageAcceptsRequestAndResponse(t *testing.T) {
	for _, fixture := range []string{providerRequestFixture, providerResponseFixture} {
		if err := ValidateLanguageProviderMessage([]byte(fixture)); err != nil {
			t.Fatalf("valid provider message rejected: %v", err)
		}
	}
}

func TestValidateLanguageProviderMessageRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	tests := map[string]string{
		"duplicate member":           strings.Replace(providerRequestFixture, `"language":"typescript"`, `"language":"typescript","language":"python"`, 1),
		"project plugin execution":   strings.Replace(providerRequestFixture, `"project_plugins":"never_execute"`, `"project_plugins":"execute"`, 1),
		"extra command field":        strings.Replace(providerRequestFixture, `"message_type":"analyze_request"`, `"message_type":"analyze_request","command":"target.py"`, 1),
		"wrong version":              strings.Replace(providerResponseFixture, `"schema_version":1`, `"schema_version":2`, 1),
		"missing capability version": strings.Replace(providerResponseFixture, `"version":"1.0.0"}]}`, `"version":""}]}`, 1),
		"trailing value":             providerRequestFixture + `{}`,
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateLanguageProviderMessage([]byte(fixture)); err == nil {
				t.Fatal("invalid provider message accepted")
			}
		})
	}
}
