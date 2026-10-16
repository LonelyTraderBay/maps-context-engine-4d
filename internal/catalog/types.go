// Package catalog contains validated registration and administration records.
package catalog

import "github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"

type Repository struct {
	ID                 model.UUID
	CommonDir          string
	FilesystemIdentity string
}

type Worktree struct {
	ID                 model.UUID
	RepositoryID       model.UUID
	CanonicalRoot      string
	FilesystemIdentity string
	BindingRevision    int64
	Status             string
}

type AllowedExecutable struct {
	Path   string     `json:"path"`
	SHA256 model.Hash `json:"sha256"`
}

type PolicyConfig struct {
	SchemaVersion        int64               `json:"schema_version"`
	AllowedRoots         []string            `json:"allowed_roots"`
	DeniedGlobs          []string            `json:"denied_globs"`
	TrustedRulePaths     []string            `json:"trusted_rule_paths"`
	DependencyRoots      []string            `json:"dependency_roots"`
	AllowedExecutables   []AllowedExecutable `json:"allowed_executables"`
	AllowTargetExecution bool                `json:"allow_target_execution"`
	AllowRuntimeDownload bool                `json:"allow_runtime_download"`
	MaxFileBytes         int64               `json:"max_file_bytes"`
	MaxSourceBytes       int64               `json:"max_source_bytes"`
	MaxOutputBytes       int64               `json:"max_output_bytes"`
	SecretRulesVersion   string              `json:"secret_rules_version"`
}

type PolicyVersion struct {
	WorktreeID model.UUID
	Digest     model.Hash
	Config     PolicyConfig
	Epoch      int64
}

type RegistrationBundle struct {
	Repository Repository
	Worktree   Worktree
	Policy     PolicyVersion
}

type RegistrationResult struct {
	RepositoryID    model.UUID
	WorktreeID      model.UUID
	PolicyDigest    model.Hash
	PolicyEpoch     int64
	BindingRevision int64
	AlreadyPresent  bool
}

type RebindRequest struct {
	WorktreeID              model.UUID
	ExpectedBindingRevision int64
	RepositoryCommonDir     string
	RepositoryFSIdentity    string
	CanonicalRoot           string
	WorktreeFSIdentity      string
}

type RemoveRequest struct {
	WorktreeID              model.UUID
	ExpectedBindingRevision int64
}

type EffectiveProfile struct {
	WorktreeID      model.UUID
	Name            string
	ID              model.Hash
	ConfigJSON      []byte
	ToolchainDigest model.Hash
	ProviderDigest  model.Hash
}

type ProfileMutation struct {
	WorktreeID       model.UUID
	Name             string
	ExpectedRevision int64
	Profile          EffectiveProfile
}

type ProfileHead struct {
	WorktreeID model.UUID
	Name       string
	ProfileID  model.Hash
	Revision   int64
}

// GroupMetadata and PermissionVersion mirror the immutable catalog rows. Their
// CAS and lifecycle policies are applied by the task-specific app services.
type GroupMetadata struct {
	GroupID    model.UUID
	Name       string
	Status     string
	Revision   int64
	ConfigJSON []byte
}

type PermissionVersion struct {
	ProfileID      string
	Version        int64
	DeclaredStatus string
	ConfigJSON     []byte
	ConfigDigest   model.Hash
	ActorID        string
	Reason         string
	CreatedAt      string
}
