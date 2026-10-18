package contracts

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestRequestSchemaPublishesFrozenMCPInputs(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mce4d_workspace_status",
		"mce4d_locate",
		"mce4d_context_pack",
		"mce4d_expand_pack",
		"mce4d_validate_pack",
		"mce4d_rename_preview",
	}
	for _, operation := range want {
		t.Run(operation, func(t *testing.T) {
			raw, err := validator.RequestSchema(operation)
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatalf("decode published request schema: %v", err)
			}
			if schema.Type != "object" {
				t.Fatalf("schema type = %q, want object", schema.Type)
			}
			if _, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true}); err != nil {
				t.Fatalf("resolve published request schema: %v", err)
			}
		})
	}
}

func TestRequestSchemaRejectsUnregisteredOperation(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.RequestSchema("mce4d_alias"); err == nil {
		t.Fatal("RequestSchema accepted an unregistered operation")
	}
}
