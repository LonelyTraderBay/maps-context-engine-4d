package federation

import (
	"fmt"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type ContractFamily string

const (
	ContractHTTP     ContractFamily = "http"
	ContractSQL      ContractFamily = "sql"
	ContractAsyncAPI ContractFamily = "asyncapi"
	ContractSchema   ContractFamily = "schema"
)

// ContractIdentity is the complete, sanitized identity used at a federation
// boundary. It deliberately includes the service and namespace so equal
// names in different services cannot create an implicit link.
type ContractIdentity struct {
	ServiceID       model.Identifier `json:"service_id"`
	RepositoryScope model.Identifier `json:"repository_scope"`
	Environment     model.Identifier `json:"environment"`
	ProfileID       model.Hash       `json:"profile_id"`
	Family          ContractFamily   `json:"family"`
	Protocol        model.Identifier `json:"protocol"`
	Namespace       model.Identifier `json:"namespace"`
	ServerBinding   model.Identifier `json:"server_binding"`
	ContractID      model.Identifier `json:"contract_id"`
	Version         model.Identifier `json:"version"`
	SourceHash      model.Hash       `json:"source_hash"`
}

func (identity ContractIdentity) Validate() error {
	for _, item := range []struct {
		name  string
		value model.Identifier
	}{
		{"service_id", identity.ServiceID},
		{"repository_scope", identity.RepositoryScope},
		{"environment", identity.Environment},
		{"protocol", identity.Protocol},
		{"namespace", identity.Namespace},
		{"server_binding", identity.ServerBinding},
		{"contract_id", identity.ContractID},
		{"version", identity.Version},
	} {
		if !validFederationLabel(string(item.value)) {
			return fmt.Errorf("%w: %s is invalid", ErrInvalidBridge, item.name)
		}
	}
	serverBinding := string(identity.ServerBinding)
	if strings.Contains(serverBinding, "://") || strings.ContainsAny(serverBinding, "@?#") {
		return fmt.Errorf("%w: server_binding must be an opaque identity without URL data", ErrInvalidBridge)
	}
	if _, err := model.ParseHash(string(identity.ProfileID)); err != nil {
		return fmt.Errorf("%w: profile_id: %v", ErrInvalidBridge, err)
	}
	if _, err := model.ParseHash(string(identity.SourceHash)); err != nil {
		return fmt.Errorf("%w: source_hash: %v", ErrInvalidBridge, err)
	}
	switch identity.Family {
	case ContractHTTP:
		if identity.Protocol != "openapi_3.1.0" {
			return fmt.Errorf("%w: unsupported HTTP contract protocol", ErrInvalidBridge)
		}
	case ContractSQL:
		if identity.Protocol != "mysql" {
			return fmt.Errorf("%w: unsupported SQL dialect", ErrInvalidBridge)
		}
	case ContractAsyncAPI:
		if identity.Protocol != "asyncapi_3.1.0" {
			return fmt.Errorf("%w: unsupported event-contract protocol", ErrInvalidBridge)
		}
	case ContractSchema:
		if identity.Protocol != "json_schema_2020-12" && identity.Protocol != "proto3" {
			return fmt.Errorf("%w: unsupported schema protocol", ErrInvalidBridge)
		}
	default:
		return fmt.Errorf("%w: unsupported contract family", ErrInvalidBridge)
	}
	return nil
}

func (identity ContractIdentity) Key() (model.Hash, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	return model.HashValue("federated-contract-identity-v1", map[string]any{
		"service_id":       identity.ServiceID,
		"repository_scope": identity.RepositoryScope,
		"environment":      identity.Environment,
		"profile_id":       identity.ProfileID,
		"family":           string(identity.Family),
		"protocol":         identity.Protocol,
		"namespace":        identity.Namespace,
		"server_binding":   identity.ServerBinding,
		"contract_id":      identity.ContractID,
		"version":          identity.Version,
		"source_hash":      identity.SourceHash,
	})
}
