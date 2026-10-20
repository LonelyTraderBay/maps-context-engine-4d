package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type tsProfile struct {
	SchemaVersion       int      `json:"schema_version"`
	Language            string   `json:"language"`
	TSConfigPaths       []string `json:"tsconfig_paths"`
	CompilerDigest      string   `json:"compiler_digest"`
	NodeDigest          string   `json:"node_digest"`
	ProviderDigest      string   `json:"provider_digest"`
	AllowProjectPlugins bool     `json:"allow_project_plugins"`
	AnalysisMode        string   `json:"analysis_mode"`
}

func main() {
	if len(os.Args) != 2 {
		fail("usage: profile-hash-check <profile.json>")
	}
	file, err := os.Open(os.Args[1])
	if err != nil {
		fail("open profile: %v", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var profile tsProfile
	if err := decoder.Decode(&profile); err != nil {
		fail("decode profile: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		fail("profile has trailing JSON")
	}
	if profile.SchemaVersion != 1 || profile.Language != "typescript_javascript" || profile.AllowProjectPlugins {
		fail("profile violates the closed TypeScript profile")
	}
	if profile.AnalysisMode != "syntax" && profile.AnalysisMode != "typed_when_available" {
		fail("unsupported analysis mode: %q", profile.AnalysisMode)
	}
	if len(profile.TSConfigPaths) == 0 {
		fail("profile must own at least one tsconfig path")
	}
	for _, digest := range []string{profile.CompilerDigest, profile.NodeDigest, profile.ProviderDigest} {
		if _, err := model.ParseHash(digest); err != nil {
			fail("invalid fingerprint: %v", err)
		}
	}
	sort.Slice(profile.TSConfigPaths, func(i, j int) bool {
		return bytes.Compare([]byte(profile.TSConfigPaths[i]), []byte(profile.TSConfigPaths[j])) < 0
	})
	for i, path := range profile.TSConfigPaths {
		if path == "" || (i > 0 && profile.TSConfigPaths[i-1] == path) {
			fail("empty or duplicate tsconfig path")
		}
	}
	paths := make([]any, len(profile.TSConfigPaths))
	for i, path := range profile.TSConfigPaths {
		paths[i] = path
	}
	value := map[string]any{
		"schema_version":        profile.SchemaVersion,
		"language":              profile.Language,
		"tsconfig_paths":        paths,
		"compiler_digest":       profile.CompilerDigest,
		"node_digest":           profile.NodeDigest,
		"provider_digest":       profile.ProviderDigest,
		"allow_project_plugins": profile.AllowProjectPlugins,
		"analysis_mode":         profile.AnalysisMode,
	}
	digest, err := model.HashValue("profile-v1", value)
	if err != nil {
		fail("hash profile: %v", err)
	}
	canonical, err := model.CanonicalEncode(value)
	if err != nil {
		fail("encode profile: %v", err)
	}
	fmt.Fprintf(os.Stderr, "canonical_profile=%s\n", canonical)
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"profile_hash": string(digest)}); err != nil {
		fail("write profile hash: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
