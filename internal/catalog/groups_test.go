package catalog

import (
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestValidateGroupConfigAllowsSameSourceAcrossServices(t *testing.T) {
	config := testGroupConfig()
	second := config.Members[0]
	second.ServiceID = "payments"
	config.Members = append(config.Members, second)
	if err := ValidateGroupConfig(config); err != nil {
		t.Fatalf("same worktree/profile with distinct services should remain explicit members: %v", err)
	}
}

func TestValidateGroupConfigRejectsInvalidAndCredentialBearingMembers(t *testing.T) {
	tests := map[string]func(*GroupConfig){
		"invalid group ID": func(config *GroupConfig) { config.GroupID = "not-a-uuid" },
		"empty members":    func(config *GroupConfig) { config.Members = []GroupMemberConfig{} },
		"duplicate service": func(config *GroupConfig) {
			config.Members = append(config.Members, config.Members[0])
		},
		"URL userinfo": func(config *GroupConfig) {
			secret := "https://user:password@example.test"
			config.Members[0].Namespace.BaseOrigin = &secret
		},
		"URL query": func(config *GroupConfig) {
			secret := "https://example.test?access_token=secret"
			config.Members[0].Namespace.BaseOrigin = &secret
		},
		"route query": func(config *GroupConfig) {
			secret := "/v1?access_token=secret"
			config.Members[0].Namespace.RoutePrefix = &secret
		},
		"invalid service ID": func(config *GroupConfig) {
			config.Members[0].ServiceID = model.Identifier(strings.Repeat("x", 193))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := testGroupConfig()
			mutate(&config)
			if err := ValidateGroupConfig(config); err == nil {
				t.Fatal("invalid group configuration was accepted")
			}
		})
	}
}

func testGroupConfig() GroupConfig {
	return GroupConfig{
		GroupID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "payments",
		Members: []GroupMemberConfig{{
			ServiceID: "api", WorktreeID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			ProfileID: model.Hash(strings.Repeat("c", 64)), Namespace: ServiceNamespace{Environment: "prod", Protocol: "http"},
		}},
	}
}
