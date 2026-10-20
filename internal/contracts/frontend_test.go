package contracts

import (
	"errors"
	"reflect"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	frontendTestWorktreeID   = model.UUID("33333333-3333-4333-8333-333333333333")
	frontendTestGenerationID = model.UUID("44444444-4444-4444-8444-444444444444")
	frontendTestProfileID    = model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	frontendTestContentHash  = model.Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func frontendTestService(value string) *model.Identifier {
	service := model.Identifier(value)
	return &service
}

func frontendTestFact(id model.Identifier, kind FrontendFactKind, service *model.Identifier, method, path string) FrontendFact {
	return FrontendFact{
		ID:        id,
		Kind:      kind,
		Name:      string(id),
		ServiceID: service,
		Method:    method,
		Path:      path,
		Source: model.SourceRef{
			WorktreeID:   frontendTestWorktreeID,
			ProfileID:    frontendTestProfileID,
			GenerationID: frontendTestGenerationID,
			Path:         "src/frontend.tsx",
			ContentHash:  frontendTestContentHash,
			Range:        model.ByteRange{StartByte: 0, EndByte: 1},
		},
	}
}

func TestFrontendClientLinksRequireMatchingServiceAndSourceScope(t *testing.T) {
	client := frontendTestFact("client", FrontendAPIClient, frontendTestService("catalog"), "GET", "/api/users")
	handler := frontendTestFact("handler", FrontendAPIHandler, frontendTestService("catalog"), "GET", "/api/users")
	links, err := ResolveFrontendClientHandlers([]FrontendFact{handler, client})
	if err != nil {
		t.Fatalf("resolve exact frontend link: %v", err)
	}
	if len(links) != 1 || links[0].Status != ResolutionExact || len(links[0].TargetFactIDs) != 1 || links[0].TargetFactIDs[0] != handler.ID {
		t.Fatalf("exact link = %#v", links)
	}
	if err := links[0].Validate(); err != nil {
		t.Fatalf("validate exact link: %v", err)
	}

	handler.ServiceID = frontendTestService("billing")
	links, err = ResolveFrontendClientHandlers([]FrontendFact{client, handler})
	if err != nil {
		t.Fatalf("resolve cross-service path: %v", err)
	}
	if len(links) != 1 || links[0].Status != ResolutionUnresolved || len(links[0].TargetFactIDs) != 0 {
		t.Fatalf("same path in another service must not link: %#v", links)
	}
}

func TestFrontendDuplicateHandlersRemainSortedCandidates(t *testing.T) {
	client := frontendTestFact("client", FrontendAPIClient, frontendTestService("catalog"), "POST", "/api/users")
	handlerB := frontendTestFact("handler-b", FrontendAPIHandler, frontendTestService("catalog"), "POST", "/api/users")
	handlerA := frontendTestFact("handler-a", FrontendAPIHandler, frontendTestService("catalog"), "POST", "/api/users")
	links, err := ResolveFrontendClientHandlers([]FrontendFact{handlerB, client, handlerA})
	if err != nil {
		t.Fatalf("resolve duplicate handlers: %v", err)
	}
	if len(links) != 1 || links[0].Status != ResolutionCandidate {
		t.Fatalf("duplicate handlers must be a candidate: %#v", links)
	}
	want := []model.Identifier{"handler-a", "handler-b"}
	if !reflect.DeepEqual(links[0].TargetFactIDs, want) {
		t.Fatalf("targets = %v, want %v", links[0].TargetFactIDs, want)
	}
}

func TestFrontendMissingNamespaceAndDifferentGenerationOrProfileStayUnresolved(t *testing.T) {
	client := frontendTestFact("client", FrontendAPIClient, nil, "GET", "/api/users")
	handler := frontendTestFact("handler", FrontendAPIHandler, nil, "GET", "/api/users")
	links, err := ResolveFrontendClientHandlers([]FrontendFact{client, handler})
	if err != nil {
		t.Fatalf("resolve missing namespace: %v", err)
	}
	if links[0].Status != ResolutionUnresolved || len(links[0].TargetFactIDs) != 0 {
		t.Fatalf("missing service namespace must remain unresolved: %#v", links[0])
	}

	client.ServiceID = frontendTestService("catalog")
	handler.ServiceID = frontendTestService("catalog")
	handler.Source.GenerationID = model.UUID("55555555-5555-4555-8555-555555555555")
	links, err = ResolveFrontendClientHandlers([]FrontendFact{client, handler})
	if err != nil {
		t.Fatalf("resolve another generation: %v", err)
	}
	if links[0].Status != ResolutionUnresolved {
		t.Fatalf("different generation must remain unresolved: %#v", links[0])
	}
	handler.Source.GenerationID = client.Source.GenerationID
	handler.Source.ProfileID = model.Hash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	links, err = ResolveFrontendClientHandlers([]FrontendFact{client, handler})
	if err != nil {
		t.Fatalf("resolve another profile: %v", err)
	}
	if links[0].Status != ResolutionUnresolved {
		t.Fatalf("different profile must remain unresolved: %#v", links[0])
	}
}

func TestFrontendRouteComponentLinkRequiresSameNamespaceAndGeneration(t *testing.T) {
	component := frontendTestFact("component", FrontendComponent, frontendTestService("storefront"), "", "")
	route := frontendTestFact("route", FrontendRoute, frontendTestService("storefront"), "", "/users")
	route.ComponentID = &component.ID
	if err := ValidateFrontendFacts([]FrontendFact{route, component}); err != nil {
		t.Fatalf("valid route-to-component source link rejected: %v", err)
	}

	otherService := frontendTestFact("other-component", FrontendComponent, frontendTestService("billing"), "", "")
	route.ComponentID = &otherService.ID
	if err := ValidateFrontendFacts([]FrontendFact{route, otherService}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("cross-service component link error = %v", err)
	}
}

func TestFrontendFactsRejectNonCanonicalHTTPPathAndMethod(t *testing.T) {
	fact := frontendTestFact("client", FrontendAPIClient, frontendTestService("catalog"), "get", "/api/users")
	if err := fact.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("lowercase method error = %v", err)
	}
	fact.Method = "GET"
	fact.Path = "/api/users?include=profile"
	if err := fact.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("query-bearing path error = %v", err)
	}
}

func TestFrontendResolutionIsDeterministicAcrossInputOrder(t *testing.T) {
	client := frontendTestFact("client", FrontendAPIClient, frontendTestService("catalog"), "GET", "/api/users")
	handlerA := frontendTestFact("handler-a", FrontendAPIHandler, frontendTestService("catalog"), "GET", "/api/users")
	handlerB := frontendTestFact("handler-b", FrontendAPIHandler, frontendTestService("catalog"), "GET", "/api/users")
	first, err := ResolveFrontendClientHandlers([]FrontendFact{client, handlerA, handlerB})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveFrontendClientHandlers([]FrontendFact{handlerB, handlerA, client})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("resolution depends on input order:\n%#v\n%#v", first, second)
	}
}
