package contracts

import (
	"strings"
	"testing"
)

const providerRequestFixture = `{"schema_version":1,"message_type":"analyze_request","request_id":"123e4567-e89b-12d3-a456-426614174000","language":"typescript","profile":{"schema_id":"typescript-profile","schema_version":1,"profile_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","config":{"analysis_mode":"syntax"}},"files":[{"path":"src/main.ts","content_base64":"YQ==","content_sha256":"ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"}],"execution_policy":{"target_code":"never_execute","project_scripts":"never_execute","project_plugins":"never_execute","notebook_cells":"never_execute","network":false}}`

const providerResponseFixture = `{"schema_version":1,"message_type":"analyze_response","request_id":"123e4567-e89b-12d3-a456-426614174000","provider":{"id":"typescript-v6","version":"1.0.0","license":"Apache-2.0","digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","languages":["typescript"],"capabilities":[{"id":"symbols.syntax","version":"1.0.0"}]},"symbols":[],"relations":[],"diagnostics":[]}`

const pythonTypedResponseFixture = `{"schema_version":1,"message_type":"analyze_response","request_id":"123e4567-e89b-12d3-a456-426614174000","provider":{"id":"python-v1","version":"1.0.0","license":"Apache-2.0","digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","languages":["python"],"capabilities":[{"id":"relations.typed_annotations","version":"1.0.0"}]},"symbols":[{"id":"sym:function","kind":"function","name":"load","path":"src/app.py","range":{"start_byte":0,"end_byte":12},"analysis_status":"syntax"},{"id":"sym:type","kind":"type_annotation","name":"User","path":"src/app.py","range":{"start_byte":10,"end_byte":14},"analysis_status":"typed"}],"relations":[{"id":"rel:annotation","source_symbol_id":"sym:function","target_symbol_id":"sym:type","kind":"typed_annotation","evidence_kind":"declared","path":"src/app.py","range":{"start_byte":10,"end_byte":14},"analysis_status":"typed"}],"diagnostics":[{"code":"TYPED_DECLARATIONS_ONLY","severity":"info","message":"Only inert declared annotation expressions are typed.","path":"src/app.py","range":{"start_byte":0,"end_byte":0}}]}`

func TestValidateLanguageProviderMessageAcceptsRequestAndResponse(t *testing.T) {
	for _, fixture := range []string{providerRequestFixture, providerResponseFixture} {
		if err := ValidateLanguageProviderMessage([]byte(fixture)); err != nil {
			t.Fatalf("valid provider message rejected: %v", err)
		}
	}
}

func TestValidateLanguageProviderMessageAcceptsDeclaredTypedPythonFacts(t *testing.T) {
	if err := ValidateLanguageProviderMessage([]byte(pythonTypedResponseFixture)); err != nil {
		t.Fatalf("valid declared typed Python response rejected: %v", err)
	}
	for name, fixture := range map[string]string{
		"unknown analysis status": strings.Replace(pythonTypedResponseFixture, `"analysis_status":"typed"`, `"analysis_status":"inferred"`, 1),
		"unknown evidence kind":   strings.Replace(pythonTypedResponseFixture, `"evidence_kind":"declared"`, `"evidence_kind":"runtime"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateLanguageProviderMessage([]byte(fixture)); err == nil {
				t.Fatal("invalid typed evidence accepted")
			}
		})
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
