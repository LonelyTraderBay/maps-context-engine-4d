package registration

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type fixedVerifier struct {
	checkout VerifiedCheckout
	calls    int
	err      error
}

func (verifier *fixedVerifier) VerifyCheckout(context.Context, string) (VerifiedCheckout, error) {
	verifier.calls++
	return verifier.checkout, verifier.err
}

type fixedIDs struct {
	values []model.UUID
	index  int
}

func (ids *fixedIDs) NewUUID() (model.UUID, error) {
	if ids.index >= len(ids.values) {
		return "", errors.New("no test UUID remains")
	}
	value := ids.values[ids.index]
	ids.index++
	return value, nil
}

type catalogSpy struct {
	bundle          catalog.RegistrationBundle
	registerCalls   int
	rebind          catalog.RebindRequest
	rebindCalls     int
	remove          catalog.RemoveRequest
	removeCalls     int
	profile         catalog.ProfileMutation
	profileCalls    int
	result          catalog.RegistrationResult
	registrationErr error
}

func (store *catalogSpy) Register(_ context.Context, bundle catalog.RegistrationBundle) (catalog.RegistrationResult, error) {
	store.registerCalls++
	store.bundle = bundle
	return store.result, store.registrationErr
}

func (store *catalogSpy) Rebind(_ context.Context, request catalog.RebindRequest) (catalog.Worktree, error) {
	store.rebindCalls++
	store.rebind = request
	return catalog.Worktree{
		ID: request.WorktreeID, BindingRevision: request.ExpectedBindingRevision + 1,
		Status: "registered", CanonicalRoot: request.CanonicalRoot,
		FilesystemIdentity: request.WorktreeFSIdentity,
	}, nil
}

func (store *catalogSpy) Remove(_ context.Context, request catalog.RemoveRequest) (catalog.Worktree, error) {
	store.removeCalls++
	store.remove = request
	return catalog.Worktree{ID: request.WorktreeID, BindingRevision: request.ExpectedBindingRevision + 1, Status: "removed"}, nil
}

func (store *catalogSpy) SetProfile(_ context.Context, mutation catalog.ProfileMutation) (catalog.ProfileHead, error) {
	store.profileCalls++
	store.profile = mutation
	return catalog.ProfileHead{WorktreeID: mutation.WorktreeID, Name: mutation.Name, ProfileID: mutation.Profile.ID, Revision: mutation.ExpectedRevision + 1}, nil
}

func validCheckout(t *testing.T) VerifiedCheckout {
	t.Helper()
	root := t.TempDir()
	gitDigest, _ := model.ParseHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	return VerifiedCheckout{
		CanonicalRoot:                root,
		RootFilesystemIdentity:       "volume-1:file-11",
		CanonicalCommonDir:           filepath.Join(root, ".git"),
		RepositoryFilesystemIdentity: "volume-1:file-10",
		GitExecutablePath:            filepath.Join(root, "git.exe"),
		GitExecutableSHA256:          gitDigest,
	}
}

func newTestService(t *testing.T, checkout VerifiedCheckout, ids []model.UUID) (*Service, *fixedVerifier, *catalogSpy) {
	t.Helper()
	verifier := &fixedVerifier{checkout: checkout}
	store := &catalogSpy{result: catalog.RegistrationResult{
		RepositoryID: "11111111-1111-4111-8111-111111111111",
		WorktreeID:   "22222222-2222-4222-8222-222222222222",
		PolicyDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PolicyEpoch:  1, BindingRevision: 1,
	}}
	service, err := NewService(verifier, &fixedIDs{values: ids}, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, verifier, store
}

func TestRegisterBuildsSingleAtomicEpochOneBundleWithoutProfile(t *testing.T) {
	checkout := validCheckout(t)
	service, verifier, store := newTestService(t, checkout, []model.UUID{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	})
	result, err := service.Register(context.Background(), checkout.CanonicalRoot)
	if err != nil {
		t.Fatal(err)
	}
	if verifier.calls != 1 || store.registerCalls != 1 || result.PolicyEpoch != 1 {
		t.Fatalf("registration did not produce one epoch-1 transaction: result=%#v calls=%d/%d", result, verifier.calls, store.registerCalls)
	}
	bundle := store.bundle
	if bundle.Repository.ID == bundle.Worktree.ID || bundle.Worktree.Status != "registered" || bundle.Worktree.BindingRevision != 1 {
		t.Fatalf("repository/worktree identities or initial binding are invalid: %#v", bundle)
	}
	if bundle.Policy.WorktreeID != bundle.Worktree.ID || bundle.Policy.Epoch != 1 || len(bundle.Policy.Config.AllowedRoots) != 1 || bundle.Policy.Config.AllowedRoots[0] != checkout.CanonicalRoot {
		t.Fatalf("initial policy is not bound to the verified root: %#v", bundle.Policy)
	}
	if len(bundle.Policy.Config.AllowedExecutables) != 1 || bundle.Policy.Config.AllowedExecutables[0].Path != checkout.GitExecutablePath || bundle.Policy.Config.AllowTargetExecution {
		t.Fatalf("initial executable allowlist widened: %#v", bundle.Policy.Config)
	}
}

func TestRegisterRejectsImplicitOrUnverifiedRootsBeforeCatalogWrite(t *testing.T) {
	checkout := validCheckout(t)
	service, verifier, store := newTestService(t, checkout, []model.UUID{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	})
	if _, err := service.Register(context.Background(), "relative/path"); err == nil {
		t.Fatal("relative root accepted")
	}
	if verifier.calls != 0 || store.registerCalls != 0 {
		t.Fatal("invalid root reached the verifier or catalog")
	}
	checkout.GitExecutableSHA256 = "not-a-hash"
	service, verifier, store = newTestService(t, checkout, []model.UUID{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	})
	if _, err := service.Register(context.Background(), checkout.CanonicalRoot); err == nil {
		t.Fatal("unverified Git executable digest accepted")
	}
	if verifier.calls != 1 || store.registerCalls != 0 {
		t.Fatal("invalid verifier output reached catalog transaction")
	}
}

func TestRegisterRejectsCollidingIDsAndCatalogErrorsWithoutRetry(t *testing.T) {
	checkout := validCheckout(t)
	id := model.UUID("11111111-1111-4111-8111-111111111111")
	service, _, store := newTestService(t, checkout, []model.UUID{id, id})
	if _, err := service.Register(context.Background(), checkout.CanonicalRoot); err == nil {
		t.Fatal("repo and worktree ID collision accepted")
	}
	if store.registerCalls != 0 {
		t.Fatal("ID collision reached catalog")
	}
	service, _, store = newTestService(t, checkout, []model.UUID{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	})
	store.registrationErr = errors.New("identity already bound")
	if _, err := service.Register(context.Background(), checkout.CanonicalRoot); err == nil {
		t.Fatal("catalog transaction error was ignored")
	}
	if store.registerCalls != 1 {
		t.Fatalf("catalog transaction was retried: %d calls", store.registerCalls)
	}
}

func TestRebindAndRemoveRequireCASRevisionAndUseTombstonePort(t *testing.T) {
	checkout := validCheckout(t)
	service, _, store := newTestService(t, checkout, nil)
	worktreeID := model.UUID("22222222-2222-4222-8222-222222222222")
	updated, err := service.Rebind(context.Background(), worktreeID, 4, checkout.CanonicalRoot)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BindingRevision != 5 || store.rebindCalls != 1 || store.rebind.RepositoryCommonDir != checkout.CanonicalCommonDir {
		t.Fatalf("rebind did not forward identity and CAS revision: %#v %#v", updated, store.rebind)
	}
	if _, err := service.Remove(context.Background(), worktreeID, 5); err != nil {
		t.Fatal(err)
	}
	if store.removeCalls != 1 || store.remove.ExpectedBindingRevision != 5 {
		t.Fatalf("remove did not use catalog tombstone CAS: %#v", store.remove)
	}
	if _, err := service.Remove(context.Background(), worktreeID, 0); err == nil {
		t.Fatal("zero revision accepted for remove")
	}
}

func TestRandomUUIDSourceProducesCanonicalVersionFourIDs(t *testing.T) {
	first, err := (RandomUUIDSource{}).NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.ParseUUID(string(first)); err != nil || first[14] != '4' || first[19] < '8' || first[19] > 'b' {
		t.Fatalf("generated UUID is not canonical v4: %q (%v)", first, err)
	}
}

func TestSetProfileValidatesNormalizesHashesAndUsesHeadCAS(t *testing.T) {
	service, _, store := newTestService(t, validCheckout(t), nil)
	worktreeID := model.UUID("22222222-2222-4222-8222-222222222222")
	profile := []byte(`{"schema_version":1,"language":"go","goos":"windows","goarch":"amd64","build_tags":["zeta","alpha"],"cgo_enabled":false,"tests_enabled":true,"module_roots":["src","cmd"],"toolchain_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provider_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","module_mode":"readonly","analysis_mode":"syntax","dependency_roots":["vendor","shared"]}`)
	head, err := service.SetProfile(context.Background(), worktreeID, "default", 3, profile)
	if err != nil {
		t.Fatal(err)
	}
	if store.profileCalls != 1 || store.profile.ExpectedRevision != 3 || head.Revision != 4 || head.ProfileID != store.profile.Profile.ID {
		t.Fatalf("profile head was not advanced through CAS: %#v %#v", head, store.profile)
	}
	if string(store.profile.Profile.ConfigJSON) != `{"analysis_mode":"syntax","build_tags":["alpha","zeta"],"cgo_enabled":false,"dependency_roots":["shared","vendor"],"goarch":"amd64","goos":"windows","language":"go","module_mode":"readonly","module_roots":["cmd","src"],"provider_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","schema_version":1,"tests_enabled":true,"toolchain_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}` {
		t.Fatalf("profile set did not canonicalize identity arrays: %s", store.profile.Profile.ConfigJSON)
	}
}

func TestSetProfileRejectsDuplicateIdentityArraysAndBadDigestWithoutWrite(t *testing.T) {
	service, _, store := newTestService(t, validCheckout(t), nil)
	worktreeID := model.UUID("22222222-2222-4222-8222-222222222222")
	duplicate := []byte(`{"schema_version":1,"language":"go","goos":"windows","goarch":"amd64","build_tags":["same","same"],"cgo_enabled":false,"tests_enabled":true,"module_roots":["src"],"toolchain_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provider_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","module_mode":"readonly","analysis_mode":"syntax","dependency_roots":[]}`)
	if _, err := service.SetProfile(context.Background(), worktreeID, "default", 0, duplicate); err == nil {
		t.Fatal("duplicate profile identity array accepted")
	}
	badHash := []byte(`{"schema_version":1,"language":"go","goos":"windows","goarch":"amd64","build_tags":[],"cgo_enabled":false,"tests_enabled":true,"module_roots":["src"],"toolchain_digest":"bad","provider_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","module_mode":"readonly","analysis_mode":"syntax","dependency_roots":[]}`)
	if _, err := service.SetProfile(context.Background(), worktreeID, "default", 0, badHash); err == nil {
		t.Fatal("invalid profile digest accepted")
	}
	if store.profileCalls != 0 {
		t.Fatal("invalid profile reached catalog CAS")
	}
}

func TestTypeScriptProfileKeepsCompilerNodeAndProviderIdentity(t *testing.T) {
	service, _, store := newTestService(t, validCheckout(t), nil)
	worktreeID := model.UUID("22222222-2222-4222-8222-222222222222")
	profile := []byte(`{"schema_version":1,"language":"typescript_javascript","tsconfig_paths":["packages/z/tsconfig.json","packages/a/tsconfig.json"],"compiler_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","node_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","provider_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","allow_project_plugins":false,"analysis_mode":"typed_when_available"}`)
	if _, err := service.SetProfile(context.Background(), worktreeID, "web", 0, profile); err != nil {
		t.Fatal(err)
	}
	if store.profile.Profile.ToolchainDigest != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || store.profile.Profile.ProviderDigest != "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("TypeScript identity fingerprints were lost: %#v", store.profile.Profile)
	}
	if len(store.profile.Profile.ConfigJSON) > 0 && !bytes.Contains(store.profile.Profile.ConfigJSON, []byte("node_digest")) {
		t.Fatal("node digest omitted from immutable profile config")
	}
}
