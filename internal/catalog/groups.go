package catalog

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type ServiceNamespace struct {
	Environment     model.Identifier  `json:"environment"`
	Protocol        string            `json:"protocol"`
	BaseOrigin      *string           `json:"base_origin"`
	RoutePrefix     *string           `json:"route_prefix"`
	ContractVersion *model.Identifier `json:"contract_version"`
}

type GroupMemberConfig struct {
	ServiceID  model.Identifier `json:"service_id"`
	WorktreeID model.UUID       `json:"worktree_id"`
	ProfileID  model.Hash       `json:"profile_id"`
	Namespace  ServiceNamespace `json:"namespace"`
}

type GroupConfig struct {
	GroupID model.UUID          `json:"group_id"`
	Name    string              `json:"name"`
	Members []GroupMemberConfig `json:"members"`
}

type GroupSummary struct {
	Config   GroupConfig `json:"config"`
	Revision int64       `json:"revision"`
	Status   string      `json:"status"`
}

// GroupSnapshot binds group metadata to the catalog incarnation read in the
// same database snapshot. It is the only input accepted by live scope resolve.
type GroupSnapshot struct {
	Group              GroupSummary
	CatalogIncarnation model.UUID
}

type GroupListPage struct {
	Groups             []GroupSummary
	CatalogIncarnation model.UUID
	CatalogWatermark   string
	HasMore            bool
	NextCursor         string
}

// ValidateGroupConfig checks the closed S43 group fields before a store opens
// a write transaction. Names are labels only; this validator never links
// members by a matching name.
func ValidateGroupConfig(config GroupConfig) error {
	if _, err := model.ParseUUID(string(config.GroupID)); err != nil {
		return fmt.Errorf("group ID: %w", err)
	}
	if !validLabel(config.Name, 256) {
		return fmt.Errorf("group name is empty, oversized, or malformed: %w", model.ErrInvalidValue)
	}
	if len(config.Members) < 1 || len(config.Members) > 32 {
		return fmt.Errorf("group members must contain 1..32 entries: %w", model.ErrInvalidValue)
	}
	services := make(map[model.Identifier]struct{}, len(config.Members))
	for index, member := range config.Members {
		if !validLabel(string(member.ServiceID), 192) {
			return fmt.Errorf("member %d service ID is invalid: %w", index, model.ErrInvalidValue)
		}
		if _, exists := services[member.ServiceID]; exists {
			return fmt.Errorf("member service IDs must be unique within a group: %w", model.ErrInvalidValue)
		}
		services[member.ServiceID] = struct{}{}
		if _, err := model.ParseUUID(string(member.WorktreeID)); err != nil {
			return fmt.Errorf("member %d worktree ID: %w", index, err)
		}
		if _, err := model.ParseHash(string(member.ProfileID)); err != nil {
			return fmt.Errorf("member %d profile ID: %w", index, err)
		}
		if err := member.Namespace.Validate(); err != nil {
			return fmt.Errorf("member %d namespace: %w", index, err)
		}
	}
	return nil
}

func (namespace ServiceNamespace) Validate() error {
	if !validLabel(string(namespace.Environment), 192) {
		return fmt.Errorf("environment is invalid: %w", model.ErrInvalidValue)
	}
	switch namespace.Protocol {
	case "http", "websocket", "event", "sql":
	default:
		return fmt.Errorf("protocol is unsupported: %w", model.ErrInvalidValue)
	}
	if namespace.BaseOrigin != nil {
		if !validLabel(*namespace.BaseOrigin, 2048) || hasURLSecrets(*namespace.BaseOrigin) {
			return fmt.Errorf("base origin is malformed or contains credentials: %w", model.ErrInvalidValue)
		}
	}
	if namespace.RoutePrefix != nil && (!validLabel(*namespace.RoutePrefix, 1024) || hasURLSecrets(*namespace.RoutePrefix)) {
		return fmt.Errorf("route prefix is invalid: %w", model.ErrInvalidValue)
	}
	if namespace.ContractVersion != nil && !validLabel(string(*namespace.ContractVersion), 192) {
		return fmt.Errorf("contract version is invalid: %w", model.ErrInvalidValue)
	}
	return nil
}

func validLabel(value string, maxRunes int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes || strings.TrimSpace(value) == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func hasURLSecrets(value string) bool {
	// A namespace origin is not a credential container. Inspect URL-shaped
	// values for userinfo and reject query/fragment data where tokens often leak.
	if strings.Contains(value, "@") || strings.ContainsAny(value, "?#") {
		return true
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.User != nil || parsed.Host == "" {
			return true
		}
	}
	return false
}
