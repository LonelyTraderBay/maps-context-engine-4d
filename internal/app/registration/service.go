// Package registration plans catalog mutations from identities verified by the OS/Git adapters.
package registration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrInvalidRegistration = errors.New("invalid registration request")
	ErrIdentityConflict    = errors.New("registration identity conflict")
)

type VerifiedCheckout struct {
	CanonicalRoot                string
	RootFilesystemIdentity       string
	CanonicalCommonDir           string
	RepositoryFilesystemIdentity string
	GitExecutablePath            string
	GitExecutableSHA256          model.Hash
}

type RootVerifier interface {
	// VerifyCheckout must perform the T005 secure-root and sanitized-Git checks.
	// It must not read source files or create project state.
	VerifyCheckout(ctx context.Context, requestedRoot string) (VerifiedCheckout, error)
}

type UUIDSource interface {
	NewUUID() (model.UUID, error)
}

type RandomUUIDSource struct{}

func (RandomUUIDSource) NewUUID() (model.UUID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read UUID entropy: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return model.ParseUUID(encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:])
}

type Catalog interface {
	// Register applies repository, worktree, policy row, and epoch-1 head in one
	// catalog transaction. Unique identities are reconciled there; it never
	// overwrites an existing policy or creates a profile/index.
	Register(ctx context.Context, bundle catalog.RegistrationBundle) (catalog.RegistrationResult, error)
	// Rebind CAS-updates the root binding and revision in one transaction. The
	// adapter must reject a repository identity change and never remap by name.
	Rebind(ctx context.Context, request catalog.RebindRequest) (catalog.Worktree, error)
	// Remove writes a tombstone under the expected binding revision; it must not
	// cascade-delete durable catalog, profile, or record rows.
	Remove(ctx context.Context, request catalog.RemoveRequest) (catalog.Worktree, error)
	// SetProfile inserts an immutable profile and advances its named head in the
	// same transaction using expected revision (zero means no head exists).
	SetProfile(ctx context.Context, mutation catalog.ProfileMutation) (catalog.ProfileHead, error)
}

type Service struct {
	verifier  RootVerifier
	ids       UUIDSource
	catalog   Catalog
	validator *contracts.Validator
}

func NewService(verifier RootVerifier, ids UUIDSource, store Catalog) (*Service, error) {
	if verifier == nil || ids == nil || store == nil {
		return nil, fmt.Errorf("%w: verifier, ID source, and catalog are required", ErrInvalidRegistration)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("create catalog contract validator: %w", err)
	}
	return &Service{verifier: verifier, ids: ids, catalog: store, validator: validator}, nil
}

func (service *Service) Register(ctx context.Context, requestedRoot string) (catalog.RegistrationResult, error) {
	if requestedRoot == "" || !filepath.IsAbs(requestedRoot) || !validText(requestedRoot, 4096) {
		return catalog.RegistrationResult{}, fmt.Errorf("%w: an explicit absolute root is required", ErrInvalidRegistration)
	}
	checkout, err := service.verifier.VerifyCheckout(ctx, requestedRoot)
	if err != nil {
		return catalog.RegistrationResult{}, fmt.Errorf("verify requested checkout: %w", err)
	}
	if err := validateCheckout(checkout); err != nil {
		return catalog.RegistrationResult{}, err
	}
	repositoryID, err := service.newID()
	if err != nil {
		return catalog.RegistrationResult{}, err
	}
	worktreeID, err := service.newID()
	if err != nil {
		return catalog.RegistrationResult{}, err
	}
	if repositoryID == worktreeID {
		return catalog.RegistrationResult{}, fmt.Errorf("%w: repository and worktree IDs must be distinct", ErrIdentityConflict)
	}
	policyConfig, err := catalog.DefaultPolicy(checkout.CanonicalRoot, checkout.GitExecutablePath, checkout.GitExecutableSHA256)
	if err != nil {
		return catalog.RegistrationResult{}, err
	}
	policyDigest, err := policyConfig.Digest()
	if err != nil {
		return catalog.RegistrationResult{}, err
	}
	bundle := catalog.RegistrationBundle{
		Repository: catalog.Repository{
			ID: repositoryID, CommonDir: checkout.CanonicalCommonDir,
			FilesystemIdentity: checkout.RepositoryFilesystemIdentity,
		},
		Worktree: catalog.Worktree{
			ID: worktreeID, RepositoryID: repositoryID, CanonicalRoot: checkout.CanonicalRoot,
			FilesystemIdentity: checkout.RootFilesystemIdentity, BindingRevision: 1, Status: "registered",
		},
		Policy: catalog.PolicyVersion{
			WorktreeID: worktreeID, Digest: policyDigest, Config: policyConfig, Epoch: 1,
		},
	}
	result, err := service.catalog.Register(ctx, bundle)
	if err != nil {
		return catalog.RegistrationResult{}, err
	}
	if _, err := model.ParseUUID(string(result.RepositoryID)); err != nil {
		return catalog.RegistrationResult{}, fmt.Errorf("%w: catalog returned invalid repository ID", ErrIdentityConflict)
	}
	if _, err := model.ParseUUID(string(result.WorktreeID)); err != nil || result.RepositoryID == result.WorktreeID {
		return catalog.RegistrationResult{}, fmt.Errorf("%w: catalog returned invalid worktree identity", ErrIdentityConflict)
	}
	if _, err := model.ParseHash(string(result.PolicyDigest)); err != nil || result.PolicyEpoch < 1 || result.PolicyEpoch > model.MaxSafeInteger || result.BindingRevision < 1 || result.BindingRevision > model.MaxSafeInteger {
		return catalog.RegistrationResult{}, fmt.Errorf("%w: catalog returned invalid registration revision or policy", ErrIdentityConflict)
	}
	return result, nil
}

func (service *Service) Rebind(ctx context.Context, worktreeID model.UUID, expectedRevision int64, requestedRoot string) (catalog.Worktree, error) {
	if _, err := model.ParseUUID(string(worktreeID)); err != nil {
		return catalog.Worktree{}, err
	}
	if expectedRevision < 1 || expectedRevision >= model.MaxSafeInteger || requestedRoot == "" || !filepath.IsAbs(requestedRoot) || !validText(requestedRoot, 4096) {
		return catalog.Worktree{}, fmt.Errorf("%w: binding ID, revision, and absolute root are required", ErrInvalidRegistration)
	}
	checkout, err := service.verifier.VerifyCheckout(ctx, requestedRoot)
	if err != nil {
		return catalog.Worktree{}, fmt.Errorf("verify rebind checkout: %w", err)
	}
	if err := validateCheckout(checkout); err != nil {
		return catalog.Worktree{}, err
	}
	updated, err := service.catalog.Rebind(ctx, catalog.RebindRequest{
		WorktreeID: worktreeID, ExpectedBindingRevision: expectedRevision,
		RepositoryCommonDir:  checkout.CanonicalCommonDir,
		RepositoryFSIdentity: checkout.RepositoryFilesystemIdentity,
		CanonicalRoot:        checkout.CanonicalRoot, WorktreeFSIdentity: checkout.RootFilesystemIdentity,
	})
	if err != nil {
		return catalog.Worktree{}, err
	}
	if updated.ID != worktreeID || updated.BindingRevision != expectedRevision+1 || updated.Status != "registered" || updated.CanonicalRoot != checkout.CanonicalRoot || updated.FilesystemIdentity != checkout.RootFilesystemIdentity {
		return catalog.Worktree{}, fmt.Errorf("%w: catalog rebind result did not match its CAS request", ErrIdentityConflict)
	}
	return updated, nil
}

func (service *Service) Remove(ctx context.Context, worktreeID model.UUID, expectedRevision int64) (catalog.Worktree, error) {
	if _, err := model.ParseUUID(string(worktreeID)); err != nil {
		return catalog.Worktree{}, err
	}
	if expectedRevision < 1 || expectedRevision >= model.MaxSafeInteger {
		return catalog.Worktree{}, fmt.Errorf("%w: positive binding revision is required", ErrInvalidRegistration)
	}
	removed, err := service.catalog.Remove(ctx, catalog.RemoveRequest{WorktreeID: worktreeID, ExpectedBindingRevision: expectedRevision})
	if err != nil {
		return catalog.Worktree{}, err
	}
	if removed.ID != worktreeID || removed.BindingRevision != expectedRevision+1 || removed.Status != "removed" {
		return catalog.Worktree{}, fmt.Errorf("%w: catalog remove result did not confirm tombstone CAS", ErrIdentityConflict)
	}
	return removed, nil
}

func (service *Service) SetProfile(ctx context.Context, worktreeID model.UUID, name string, expectedRevision int64, rawProfile []byte) (catalog.ProfileHead, error) {
	if _, err := model.ParseUUID(string(worktreeID)); err != nil {
		return catalog.ProfileHead{}, err
	}
	if expectedRevision < 0 || expectedRevision >= model.MaxSafeInteger {
		return catalog.ProfileHead{}, fmt.Errorf("%w: expected profile revision cannot be advanced", ErrInvalidRegistration)
	}
	value, err := contracts.DecodeJSON(rawProfile)
	if err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("%w: invalid profile JSON: %v", ErrInvalidRegistration, err)
	}
	profileValue, ok := value.(map[string]any)
	if !ok {
		return catalog.ProfileHead{}, fmt.Errorf("%w: effective profile must be an object", ErrInvalidRegistration)
	}
	profileJSON, err := model.CanonicalJSON1(profileValue)
	if err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("%w: profile identity values: %v", ErrInvalidRegistration, err)
	}
	requestJSON, err := json.Marshal(struct {
		WorktreeID       model.UUID      `json:"worktree_id"`
		ProfileName      string          `json:"profile_name"`
		ExpectedRevision int64           `json:"expected_revision"`
		Profile          json.RawMessage `json:"profile"`
	}{worktreeID, name, expectedRevision, profileJSON})
	if err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("encode profile request: %w", err)
	}
	if err := service.validator.ValidateRequest("CLI profile set", requestJSON); err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("%w: %v", ErrInvalidRegistration, err)
	}
	if err := normalizeProfileArrays(profileValue); err != nil {
		return catalog.ProfileHead{}, err
	}
	canonicalProfile, err := model.CanonicalJSON1(profileValue)
	if err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("%w: canonicalize profile: %v", ErrInvalidRegistration, err)
	}
	profileDigest, err := model.HashValue("profile-v1", profileValue)
	if err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("%w: hash profile: %v", ErrInvalidRegistration, err)
	}
	language, _ := profileValue["language"].(string)
	toolchainField := "toolchain_digest"
	if language == "typescript_javascript" {
		// The catalog scalar tracks the compiler; node_digest remains in the
		// immutable profile JSON and profile_id.
		toolchainField = "compiler_digest"
	}
	toolchainText, _ := profileValue[toolchainField].(string)
	toolchainDigest, err := model.ParseHash(toolchainText)
	if err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("%w: invalid toolchain digest", ErrInvalidRegistration)
	}
	providerText, _ := profileValue["provider_digest"].(string)
	providerDigest, err := model.ParseHash(providerText)
	if err != nil {
		return catalog.ProfileHead{}, fmt.Errorf("%w: invalid provider digest", ErrInvalidRegistration)
	}
	mutation := catalog.ProfileMutation{
		WorktreeID: worktreeID, Name: name, ExpectedRevision: expectedRevision,
		Profile: catalog.EffectiveProfile{
			WorktreeID: worktreeID, Name: name, ID: profileDigest,
			ConfigJSON: canonicalProfile, ToolchainDigest: toolchainDigest, ProviderDigest: providerDigest,
		},
	}
	head, err := service.catalog.SetProfile(ctx, mutation)
	if err != nil {
		return catalog.ProfileHead{}, err
	}
	if head.WorktreeID != worktreeID || head.Name != name || head.ProfileID != profileDigest || head.Revision != expectedRevision+1 {
		return catalog.ProfileHead{}, fmt.Errorf("%w: catalog profile head did not match its CAS request", ErrIdentityConflict)
	}
	return head, nil
}

func normalizeProfileArrays(profile map[string]any) error {
	for _, key := range []string{"build_tags", "module_roots", "dependency_roots", "tsconfig_paths"} {
		value, exists := profile[key]
		if !exists {
			continue
		}
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%w: profile %s must be an array", ErrInvalidRegistration, key)
		}
		values := make([]string, len(array))
		for index, item := range array {
			text, ok := item.(string)
			if !ok {
				return fmt.Errorf("%w: profile %s contains a non-string", ErrInvalidRegistration, key)
			}
			values[index] = text
		}
		sort.Strings(values)
		for index := 1; index < len(values); index++ {
			if values[index-1] == values[index] {
				return fmt.Errorf("%w: profile %s contains a duplicate", ErrInvalidRegistration, key)
			}
		}
		normalized := make([]any, len(values))
		for index, text := range values {
			normalized[index] = text
		}
		profile[key] = normalized
	}
	return nil
}

func (service *Service) newID() (model.UUID, error) {
	value, err := service.ids.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate catalog identity: %w", err)
	}
	parsed, err := model.ParseUUID(string(value))
	if err != nil {
		return "", fmt.Errorf("%w: ID source returned invalid UUID", ErrIdentityConflict)
	}
	return parsed, nil
}

func validateCheckout(checkout VerifiedCheckout) error {
	for _, value := range []string{
		checkout.CanonicalRoot,
		checkout.RootFilesystemIdentity,
		checkout.CanonicalCommonDir,
		checkout.RepositoryFilesystemIdentity,
		checkout.GitExecutablePath,
	} {
		if !validText(value, 4096) {
			return fmt.Errorf("%w: verified checkout identity is incomplete", ErrInvalidRegistration)
		}
	}
	if !filepath.IsAbs(checkout.CanonicalRoot) || !filepath.IsAbs(checkout.CanonicalCommonDir) || !filepath.IsAbs(checkout.GitExecutablePath) {
		return fmt.Errorf("%w: verifier returned a non-absolute canonical path", ErrInvalidRegistration)
	}
	if _, err := model.ParseHash(string(checkout.GitExecutableSHA256)); err != nil {
		return fmt.Errorf("%w: Git executable digest: %v", ErrInvalidRegistration, err)
	}
	return nil
}

func validText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f || char == '\u0085' || char == '\u2028' || char == '\u2029' {
			return false
		}
	}
	return true
}
