package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const MaxFrontendFacts = 10_000

type FrontendFactKind string

const (
	FrontendComponent  FrontendFactKind = "component"
	FrontendRoute      FrontendFactKind = "route"
	FrontendAPIClient  FrontendFactKind = "api_client"
	FrontendAPIHandler FrontendFactKind = "api_handler"
)

// FrontendFact is a source-backed declaration in one immutable generation and
// profile. A missing service namespace is retained as an unlinked fact.
type FrontendFact struct {
	ID          model.Identifier  `json:"id"`
	Kind        FrontendFactKind  `json:"kind"`
	Name        string            `json:"name"`
	ServiceID   *model.Identifier `json:"service_id,omitempty"`
	Path        string            `json:"path,omitempty"`
	Method      string            `json:"method,omitempty"`
	ComponentID *model.Identifier `json:"component_id,omitempty"`
	Source      model.SourceRef   `json:"source_ref"`
}

func (fact FrontendFact) Validate() error {
	if err := validateIdentifier("frontend fact id", fact.ID); err != nil {
		return err
	}
	if !validText(fact.Name) || len(fact.Name) > 4096 || strings.ContainsAny(fact.Name, "\x00\r\n") {
		return fmt.Errorf("%w: frontend fact name must be nonempty bounded UTF-8 without controls", ErrInvalidContract)
	}
	if fact.ServiceID != nil {
		if err := validateIdentifier("frontend service_id", *fact.ServiceID); err != nil {
			return err
		}
	}
	if err := validateSourceRef(fact.Source); err != nil {
		return err
	}
	switch fact.Kind {
	case FrontendComponent:
		if fact.Path != "" || fact.Method != "" || fact.ComponentID != nil {
			return fmt.Errorf("%w: component fact cannot carry route or operation fields", ErrInvalidContract)
		}
	case FrontendRoute:
		if err := validateFrontendPath(fact.Path); err != nil {
			return err
		}
		if fact.Method != "" {
			return fmt.Errorf("%w: frontend page route cannot carry an HTTP method", ErrInvalidContract)
		}
		if fact.ComponentID != nil {
			if err := validateIdentifier("frontend component_id", *fact.ComponentID); err != nil {
				return err
			}
		}
	case FrontendAPIClient, FrontendAPIHandler:
		if err := validateFrontendPath(fact.Path); err != nil {
			return err
		}
		if err := validateFrontendMethod(fact.Method); err != nil {
			return err
		}
		if fact.ComponentID != nil {
			return fmt.Errorf("%w: API facts cannot carry a page component link", ErrInvalidContract)
		}
	default:
		return fmt.Errorf("%w: unknown frontend fact kind", ErrInvalidContract)
	}
	return nil
}

// ValidateFrontendFacts checks identity uniqueness and local route-to-component
// links before any API client resolution is attempted.
func ValidateFrontendFacts(facts []FrontendFact) error {
	if len(facts) > MaxFrontendFacts {
		return fmt.Errorf("%w: frontend fact count exceeds the contract limit", ErrInvalidContract)
	}
	byID := make(map[model.Identifier]FrontendFact, len(facts))
	for _, fact := range facts {
		if err := fact.Validate(); err != nil {
			return err
		}
		if _, exists := byID[fact.ID]; exists {
			return fmt.Errorf("%w: duplicate frontend fact id", ErrInvalidContract)
		}
		byID[fact.ID] = fact
	}
	for _, fact := range facts {
		if fact.ComponentID == nil {
			continue
		}
		component, exists := byID[*fact.ComponentID]
		if fact.Kind != FrontendRoute || !exists || component.Kind != FrontendComponent ||
			fact.Source.GenerationID != component.Source.GenerationID ||
			fact.Source.ProfileID != component.Source.ProfileID ||
			!sameFrontendService(fact.ServiceID, component.ServiceID) {
			return fmt.Errorf("%w: frontend route component link crosses identity scope or has no component source", ErrInvalidContract)
		}
	}
	return nil
}

type FrontendLinkKind string

const FrontendClientHandlerLink FrontendLinkKind = "client_handler"

// FrontendLink keeps ambiguous handler targets as a sorted candidate set.
type FrontendLink struct {
	ID            model.Identifier   `json:"id"`
	GenerationID  model.UUID         `json:"generation_id"`
	ProfileID     model.Hash         `json:"profile_id"`
	ServiceID     *model.Identifier  `json:"service_id,omitempty"`
	SourceFactID  model.Identifier   `json:"source_fact_id"`
	TargetFactIDs []model.Identifier `json:"target_fact_ids"`
	Kind          FrontendLinkKind   `json:"kind"`
	Status        ResolutionStatus   `json:"resolution_status"`
	Evidence      []model.SourceRef  `json:"source_refs"`
}

func (link FrontendLink) Validate() error {
	if err := validateIdentifier("frontend link id", link.ID); err != nil {
		return err
	}
	if _, err := model.ParseUUID(string(link.GenerationID)); err != nil {
		return contractError("frontend link generation_id", err)
	}
	if _, err := model.ParseHash(string(link.ProfileID)); err != nil {
		return contractError("frontend link profile_id", err)
	}
	if link.ServiceID != nil {
		if err := validateIdentifier("frontend link service_id", *link.ServiceID); err != nil {
			return err
		}
	}
	if err := validateIdentifier("frontend source fact id", link.SourceFactID); err != nil {
		return err
	}
	if len(link.TargetFactIDs) > MaxFrontendFacts || len(link.Evidence) > MaxFrontendFacts+1 {
		return fmt.Errorf("%w: frontend link exceeds the candidate evidence limit", ErrInvalidContract)
	}
	if link.Kind != FrontendClientHandlerLink {
		return fmt.Errorf("%w: unknown frontend link kind", ErrInvalidContract)
	}
	switch link.Status {
	case ResolutionExact:
		if link.ServiceID == nil || len(link.TargetFactIDs) != 1 {
			return fmt.Errorf("%w: exact frontend link needs one target and a service namespace", ErrInvalidContract)
		}
	case ResolutionCandidate:
		if link.ServiceID == nil || len(link.TargetFactIDs) < 2 {
			return fmt.Errorf("%w: candidate frontend link needs multiple targets and a service namespace", ErrInvalidContract)
		}
	case ResolutionUnresolved:
		if len(link.TargetFactIDs) != 0 {
			return fmt.Errorf("%w: unresolved frontend link cannot select targets", ErrInvalidContract)
		}
	default:
		return fmt.Errorf("%w: unknown frontend link resolution status", ErrInvalidContract)
	}
	for index, targetID := range link.TargetFactIDs {
		if err := validateIdentifier("frontend target fact id", targetID); err != nil {
			return err
		}
		if index > 0 && link.TargetFactIDs[index-1] >= targetID {
			return fmt.Errorf("%w: frontend candidate targets must be unique and sorted", ErrInvalidContract)
		}
	}
	if link.Evidence == nil || len(link.Evidence) == 0 {
		return fmt.Errorf("%w: frontend link needs source evidence", ErrInvalidContract)
	}
	for _, source := range link.Evidence {
		if err := validateSourceRef(source); err != nil {
			return err
		}
		if source.GenerationID != link.GenerationID || source.ProfileID != link.ProfileID {
			return fmt.Errorf("%w: frontend link evidence crosses generation or profile", ErrInvalidContract)
		}
	}
	return nil
}

// ResolveFrontendClientHandlers links only exact method/path pairs within the
// same service, generation, and profile. Equal paths in another service stay
// unresolved; duplicate handlers remain candidates instead of choosing first.
func ResolveFrontendClientHandlers(facts []FrontendFact) ([]FrontendLink, error) {
	if err := ValidateFrontendFacts(facts); err != nil {
		return nil, err
	}
	handlers := make([]FrontendFact, 0)
	clients := make([]FrontendFact, 0)
	for _, fact := range facts {
		switch fact.Kind {
		case FrontendAPIHandler:
			handlers = append(handlers, fact)
		case FrontendAPIClient:
			clients = append(clients, fact)
		}
	}
	sort.Slice(handlers, func(i, j int) bool { return handlers[i].ID < handlers[j].ID })
	sort.Slice(clients, func(i, j int) bool { return clients[i].ID < clients[j].ID })
	links := make([]FrontendLink, 0, len(clients))
	for _, client := range clients {
		matches := make([]FrontendFact, 0)
		if client.ServiceID != nil {
			for _, handler := range handlers {
				if handler.ServiceID == nil || *handler.ServiceID != *client.ServiceID ||
					handler.Source.GenerationID != client.Source.GenerationID ||
					handler.Source.ProfileID != client.Source.ProfileID ||
					handler.Method != client.Method || handler.Path != client.Path {
					continue
				}
				matches = append(matches, handler)
			}
		}
		targetIDs := make([]model.Identifier, 0, len(matches))
		evidence := []model.SourceRef{client.Source}
		for _, match := range matches {
			targetIDs = append(targetIDs, match.ID)
			evidence = append(evidence, match.Source)
		}
		status := ResolutionUnresolved
		if len(matches) == 1 {
			status = ResolutionExact
		} else if len(matches) > 1 {
			status = ResolutionCandidate
		}
		serviceID := cloneFrontendService(client.ServiceID)
		link := FrontendLink{
			ID: frontendLinkID(string(client.Source.GenerationID), string(client.Source.ProfileID),
				frontendServiceValue(client.ServiceID), string(client.ID), string(status), strings.Join(identifierValues(targetIDs), "\x1f")),
			GenerationID:  client.Source.GenerationID,
			ProfileID:     client.Source.ProfileID,
			ServiceID:     serviceID,
			SourceFactID:  client.ID,
			TargetFactIDs: targetIDs,
			Kind:          FrontendClientHandlerLink,
			Status:        status,
			Evidence:      evidence,
		}
		if err := link.Validate(); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, nil
}

func validateFrontendPath(value string) error {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || value[0] != '/' || strings.ContainsAny(value, "?#\\") {
		return fmt.Errorf("%w: frontend path must be a static absolute path without query or fragment", ErrInvalidContract)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("%w: frontend path contains a control character", ErrInvalidContract)
		}
	}
	return nil
}

func validateFrontendMethod(value string) error {
	if value == "" || len(value) > 32 || value != strings.ToUpper(value) {
		return fmt.Errorf("%w: frontend HTTP method must be uppercase and 1..32 bytes", ErrInvalidContract)
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		valid := char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char))
		if !valid {
			return fmt.Errorf("%w: frontend HTTP method is not a token", ErrInvalidContract)
		}
	}
	return nil
}

func sameFrontendService(left, right *model.Identifier) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneFrontendService(value *model.Identifier) *model.Identifier {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func frontendServiceValue(value *model.Identifier) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

func identifierValues(values []model.Identifier) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

func frontendLinkID(values ...string) model.Identifier {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return model.Identifier("frontend-link:" + hex.EncodeToString(digest[:24]))
}
