package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	maxUIManifestBytes       = 64 << 20
	maxUIJSONNodes           = 2_000_000
	maxUIEntries             = 100_000
	maxUICapturedSourceBytes = 2 << 20 // Matches the source-capture file admission limit.
	maxUIBindingBytes        = 16 << 20
)

var ErrUIIntegrityConflict = errors.New("immutable UI metadata conflicts with existing content")

// UIBuildManifest is the generation-independent Vite producer artifact.
type UIBuildManifest struct {
	SchemaVersion       int                `json:"schema_version"`
	WorktreeID          model.UUID         `json:"worktree_id"`
	ProfileID           model.Hash         `json:"profile_id"`
	BuildMapID          model.Hash         `json:"build_map_id"`
	ProviderVersion     string             `json:"provider_version"`
	BuildConfigDigest   model.Hash         `json:"build_config_digest"`
	InputManifestDigest model.Hash         `json:"input_manifest_digest"`
	Mode                string             `json:"mode"`
	ModuleCoverage      string             `json:"module_coverage"`
	Sources             []UIManifestSource `json:"sources"`
	Anchors             []UIManifestAnchor `json:"anchors"`
	ProviderDigest      model.Hash         `json:"provider_digest"`
}

type UIManifestSource struct {
	Path        string     `json:"path"`
	ContentHash model.Hash `json:"content_hash"`
}

type UIManifestAnchor struct {
	AnchorID      model.Hash      `json:"anchor_id"`
	Kind          string          `json:"kind"`
	Path          string          `json:"path"`
	ContentHash   model.Hash      `json:"content_hash"`
	Range         model.ByteRange `json:"range"`
	OwnerAnchorID *model.Hash     `json:"owner_anchor_id"`
}

// UIBindingRequest binds an immutable producer map to one published source generation.
type UIBindingRequest struct {
	SourceGenerationID model.UUID     `json:"source_generation_id"`
	ProfileID          model.Hash     `json:"profile_id"`
	PolicyDigest       model.Hash     `json:"policy_digest"`
	ResolverDigest     model.Hash     `json:"resolver_digest"`
	Links              []UIAnchorLink `json:"links"`
}

type UIAnchorLink struct {
	LinkID           string            `json:"link_id"`
	AnchorID         model.Hash        `json:"anchor_id"`
	TargetRef        json.RawMessage   `json:"target_ref"`
	ResolutionStatus string            `json:"resolution_status"`
	Evidence         []model.SourceRef `json:"evidence"`
}

// uiTargetKind mirrors the closed GraphRef variants at this storage boundary.
// The sqlite adapter intentionally does not depend on internal/contracts.
type uiTargetKind string

const (
	uiTargetSymbol       uiTargetKind = "symbol"
	uiTargetEntity       uiTargetKind = "entity"
	uiTargetModule       uiTargetKind = "module"
	uiTargetSourceAnchor uiTargetKind = "source_anchor"
	uiTargetFile         uiTargetKind = "file"
)

type UIImportResult struct {
	BuildMapID      model.Hash `json:"build_map_id"`
	BindingID       model.Hash `json:"binding_id"`
	AlreadyImported bool       `json:"already_imported"`
}

type uiTargetRef struct {
	Kind         uiTargetKind     `json:"kind"`
	WorktreeID   model.UUID       `json:"worktree_id"`
	ProfileID    model.Hash       `json:"profile_id"`
	GenerationID model.UUID       `json:"generation_id"`
	SymbolID     model.Identifier `json:"symbol_id,omitempty"`
	EntityID     model.Identifier `json:"entity_id,omitempty"`
	ModuleID     model.Identifier `json:"module_id,omitempty"`
	Path         string           `json:"path,omitempty"`
	ContentHash  *model.Hash      `json:"content_hash"`
	BuildMapID   model.Hash       `json:"build_map_id,omitempty"`
	AnchorID     model.Hash       `json:"anchor_id,omitempty"`
	BindingID    model.Hash       `json:"binding_id,omitempty"`
}

// ImportUIManifest validates the physical artifact, source generation, targets,
// and evidence before inserting the map, anchors, binding and links atomically.
func (store *Store) ImportUIManifest(ctx context.Context, raw []byte, request UIBindingRequest) (UIImportResult, error) {
	var result UIImportResult
	if store == nil || store.writer == nil || store.ownership == nil || ctx == nil {
		return result, fmt.Errorf("UI manifest import requires a writable store and context: %w", ErrInvalidIndex)
	}
	if !store.uiEnabled {
		return result, ErrUIStoreRequired
	}
	manifest, canonical, err := decodeUIManifest(raw)
	if err != nil {
		return result, err
	}
	if manifest.WorktreeID != store.worktree || request.ProfileID != manifest.ProfileID {
		return result, fmt.Errorf("UI map and binding worktree/profile must match the opened store: %w", ErrInvalidIndex)
	}
	if _, err := model.ParseUUID(string(request.SourceGenerationID)); err != nil {
		return result, fmt.Errorf("invalid UI source generation: %w", err)
	}
	for name, digest := range map[string]model.Hash{
		"binding profile": request.ProfileID,
		"policy digest":   request.PolicyDigest,
		"resolver digest": request.ResolverDigest,
	} {
		if _, err := model.ParseHash(string(digest)); err != nil {
			return result, fmt.Errorf("invalid %s: %w", name, err)
		}
	}
	if request.ResolverDigest == manifest.ProviderDigest {
		return result, fmt.Errorf("resolver_digest must identify a different resolver artifact from provider_digest: %w", ErrInvalidIndex)
	}
	if len(request.Links) > maxUIEntries {
		return result, fmt.Errorf("UI binding link count exceeds %d: %w", maxUIEntries, ErrInvalidIndex)
	}
	bindingID, err := model.HashValue("binding-v1", map[string]any{
		"build_map_id":         manifest.BuildMapID,
		"source_generation_id": request.SourceGenerationID,
		"profile_id":           request.ProfileID,
		"policy_digest":        request.PolicyDigest,
		"resolver_digest":      request.ResolverDigest,
	})
	if err != nil {
		return result, fmt.Errorf("compute UI binding identity: %w", err)
	}
	result = UIImportResult{BuildMapID: manifest.BuildMapID, BindingID: bindingID}
	result.AlreadyImported = false
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	return result, store.withWriteTx(ctx, uiImportPeakGrowth(len(canonical), len(manifest.Anchors), len(request.Links)), func(tx *writeTx) error {
		generation, err := validateUIGeneration(ctx, tx, manifest, request)
		if err != nil {
			return err
		}
		if err := validateUIAnchorsAgainstGeneration(ctx, tx, manifest, request.SourceGenerationID); err != nil {
			return err
		}
		preparedLinks, evidenceDigest, err := prepareUILinks(store.worktree, manifest, request, bindingID, generation, tx, ctx)
		if err != nil {
			return err
		}
		mapAlreadyImported, err := insertOrVerifyUIMap(ctx, tx, manifest, canonical, model.SHA256(raw), createdAt)
		if err != nil {
			return err
		}
		bindingAlreadyImported, err := insertOrVerifyUIBinding(ctx, tx, manifest, request, bindingID, evidenceDigest, createdAt, preparedLinks)
		if err != nil {
			return err
		}
		result.AlreadyImported = mapAlreadyImported && bindingAlreadyImported
		return nil
	})
}

type uiGeneration struct {
	profile model.Hash
	policy  model.Hash
	state   string
}

func validateUIGeneration(ctx context.Context, tx *writeTx, manifest UIBuildManifest, request UIBindingRequest) (uiGeneration, error) {
	var generation uiGeneration
	err := tx.QueryRowContext(ctx, `SELECT profile_id,policy_digest,state FROM generations WHERE generation_id=?`, string(request.SourceGenerationID)).Scan(&generation.profile, &generation.policy, &generation.state)
	if errors.Is(err, sql.ErrNoRows) {
		return uiGeneration{}, fmt.Errorf("UI source generation is unavailable: %w", ErrGenerationNotFound)
	}
	if err != nil {
		return uiGeneration{}, fmt.Errorf("read UI source generation: %w", err)
	}
	if generation.profile != request.ProfileID || generation.profile != manifest.ProfileID || generation.policy != request.PolicyDigest || (generation.state != "READY" && generation.state != "READY_PARTIAL") {
		return uiGeneration{}, fmt.Errorf("UI binding does not match a published generation profile/policy: %w", ErrGenerationConflict)
	}
	for _, source := range manifest.Sources {
		var hash sql.NullString
		var status string
		var declaredLength sql.NullInt64
		var contents []byte
		err := tx.QueryRowContext(ctx, `SELECT files.content_hash,files.status,files.byte_length,content_blobs.bytes
			FROM files LEFT JOIN content_blobs ON content_blobs.content_hash=files.content_hash
			WHERE files.generation_id=? AND files.path=?`, string(request.SourceGenerationID), source.Path).
			Scan(&hash, &status, &declaredLength, &contents)
		if errors.Is(err, sql.ErrNoRows) {
			return uiGeneration{}, fmt.Errorf("UI source %q is not present in the selected generation: %w", source.Path, ErrGenerationConflict)
		}
		if err != nil {
			return uiGeneration{}, fmt.Errorf("read captured UI source %q: %w", source.Path, err)
		}
		if !hash.Valid || model.Hash(hash.String) != source.ContentHash || (status != "indexed" && status != "parse_error") || !declaredLength.Valid || declaredLength.Int64 != int64(len(contents)) || int64(len(contents)) > maxUICapturedSourceBytes || model.SHA256(contents) != source.ContentHash {
			return uiGeneration{}, fmt.Errorf("UI source %q does not match supported captured generation bytes: %w", source.Path, ErrGenerationConflict)
		}
	}
	return generation, nil
}

func validateUIAnchorsAgainstGeneration(ctx context.Context, tx *writeTx, manifest UIBuildManifest, generation model.UUID) error {
	byPath := make(map[string][]UIManifestAnchor)
	for _, anchor := range manifest.Anchors {
		byPath[anchor.Path] = append(byPath[anchor.Path], anchor)
	}
	for path, anchors := range byPath {
		var hash sql.NullString
		var status string
		var declaredLength sql.NullInt64
		var source []byte
		if err := tx.QueryRowContext(ctx, `SELECT files.content_hash,files.status,files.byte_length,content_blobs.bytes FROM files
			LEFT JOIN content_blobs ON content_blobs.content_hash=files.content_hash WHERE files.generation_id=? AND files.path=?`, string(generation), path).
			Scan(&hash, &status, &declaredLength, &source); err != nil {
			return fmt.Errorf("read UI anchor source %q: %w", path, err)
		}
		if !hash.Valid || model.Hash(hash.String) != anchors[0].ContentHash || (status != "indexed" && status != "parse_error") || !declaredLength.Valid || declaredLength.Int64 != int64(len(source)) || int64(len(source)) > maxUICapturedSourceBytes || model.SHA256(source) != anchors[0].ContentHash {
			return fmt.Errorf("UI anchor source %q does not match captured generation bytes: %w", path, ErrGenerationConflict)
		}
		for _, anchor := range anchors {
			if anchor.ContentHash != anchors[0].ContentHash {
				return fmt.Errorf("UI anchors for one path disagree on content hash: %w", ErrInvalidIndex)
			}
			if err := anchor.Range.ValidateAgainst(source); err != nil {
				return fmt.Errorf("UI anchor %s range is invalid for captured source: %w", anchor.AnchorID, err)
			}
		}
	}
	return nil
}

func validateModelSourceRef(ref model.SourceRef) error {
	if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
		return err
	}
	if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(ref.ContentHash)); err != nil {
		return err
	}
	if err := model.ValidateRelativePath(ref.Path); err != nil {
		return err
	}
	if ref.Range.StartByte < 0 || ref.Range.EndByte < ref.Range.StartByte || ref.Range.EndByte > model.MaxSafeInteger {
		return model.ErrInvalidRange
	}
	return nil
}

type preparedUILink struct {
	linkID       string
	anchorID     model.Hash
	targetJSON   string
	kind         string
	status       string
	evidenceJSON string
}

type preparedUILinks struct {
	links []preparedUILink
}

func prepareUILinks(worktree model.UUID, manifest UIBuildManifest, request UIBindingRequest, bindingID model.Hash, generation uiGeneration, tx *writeTx, ctx context.Context) (preparedUILinks, model.Hash, error) {
	anchors := make(map[model.Hash]UIManifestAnchor, len(manifest.Anchors))
	for _, anchor := range manifest.Anchors {
		anchors[anchor.AnchorID] = anchor
	}
	links := append([]UIAnchorLink(nil), request.Links...)
	sort.Slice(links, func(i, j int) bool { return links[i].LinkID < links[j].LinkID })
	prepared := preparedUILinks{links: make([]preparedUILink, 0, len(links))}
	seen := make(map[string]struct{}, len(links))
	evidenceValues := make([]any, 0, len(links))
	var bindingBytes int64
	for _, link := range links {
		if !validUIIdentifier(link.LinkID) {
			return preparedUILinks{}, "", fmt.Errorf("UI link ID is invalid: %w", ErrInvalidIndex)
		}
		if _, duplicate := seen[link.LinkID]; duplicate {
			return preparedUILinks{}, "", fmt.Errorf("duplicate UI link ID: %w", ErrInvalidIndex)
		}
		seen[link.LinkID] = struct{}{}
		anchor, ok := anchors[link.AnchorID]
		if !ok {
			return preparedUILinks{}, "", fmt.Errorf("UI link references unknown source anchor %s: %w", link.AnchorID, ErrInvalidIndex)
		}
		if len(link.Evidence) != 1 {
			return preparedUILinks{}, "", fmt.Errorf("T085 link evidence must contain exactly its verified anchor source range: %w", ErrInvalidIndex)
		}
		target, targetJSON, err := validateUITarget(ctx, tx, link.TargetRef, worktree, generation.profile, request.SourceGenerationID, manifest.BuildMapID, bindingID, anchors)
		if err != nil {
			return preparedUILinks{}, "", fmt.Errorf("validate UI link target %q: %w", link.LinkID, err)
		}
		if link.ResolutionStatus != "candidate" {
			return preparedUILinks{}, "", fmt.Errorf("T085 importer cannot assert resolver status %q without a verified resolver result: %w", link.ResolutionStatus, ErrInvalidIndex)
		}
		evidence := append([]model.SourceRef(nil), link.Evidence...)
		sort.Slice(evidence, func(i, j int) bool {
			if evidence[i].Path != evidence[j].Path {
				return evidence[i].Path < evidence[j].Path
			}
			if evidence[i].Range.StartByte != evidence[j].Range.StartByte {
				return evidence[i].Range.StartByte < evidence[j].Range.StartByte
			}
			return evidence[i].Range.EndByte < evidence[j].Range.EndByte
		})
		matchingAnchorEvidence := false
		for _, ref := range evidence {
			if ref.WorktreeID != worktree || ref.ProfileID != generation.profile || ref.GenerationID != request.SourceGenerationID {
				return preparedUILinks{}, "", fmt.Errorf("UI evidence crosses its bound generation: %w", ErrGenerationConflict)
			}
			if err := validateModelSourceRef(ref); err != nil {
				return preparedUILinks{}, "", err
			}
			if ref.Path == anchor.Path && ref.ContentHash == anchor.ContentHash && ref.Range == anchor.Range {
				matchingAnchorEvidence = true
			}
		}
		if !matchingAnchorEvidence {
			return preparedUILinks{}, "", fmt.Errorf("UI link evidence must include the exact source anchor range: %w", ErrInvalidIndex)
		}
		if target.Kind == "" {
			return preparedUILinks{}, "", fmt.Errorf("candidate UI link must have a typed target: %w", ErrInvalidIndex)
		}
		evidenceJSON, err := canonicalSourceRefs(evidence)
		if err != nil {
			return preparedUILinks{}, "", err
		}
		prepared.links = append(prepared.links, preparedUILink{
			linkID: link.LinkID, anchorID: link.AnchorID, targetJSON: targetJSON,
			kind: string(target.Kind), status: link.ResolutionStatus, evidenceJSON: string(evidenceJSON),
		})
		bindingBytes += int64(len(targetJSON) + len(evidenceJSON) + len(link.LinkID) + 256)
		if bindingBytes > maxUIBindingBytes {
			return preparedUILinks{}, "", fmt.Errorf("UI binding artifact exceeds %d bytes: %w", maxUIBindingBytes, ErrInvalidIndex)
		}
		evidenceValues = append(evidenceValues, map[string]any{
			"link_id": link.LinkID, "anchor_id": link.AnchorID, "target_ref": targetJSON,
			"kind": string(target.Kind), "resolution_status": link.ResolutionStatus, "evidence": sourceRefValues(evidence),
		})
	}
	digest, err := model.HashValue("binding-evidence-v1", evidenceValues)
	if err != nil {
		return preparedUILinks{}, "", fmt.Errorf("compute UI binding evidence digest: %w", err)
	}
	return prepared, digest, nil
}

func validateUITarget(ctx context.Context, tx *writeTx, raw json.RawMessage, worktree model.UUID, profile model.Hash, generation model.UUID, buildMapID, bindingID model.Hash, anchors map[model.Hash]UIManifestAnchor) (uiTargetRef, string, error) {
	var target uiTargetRef
	if len(raw) == 0 || len(raw) > 16<<10 {
		return target, "", fmt.Errorf("target_ref must be within 1..16384 bytes")
	}
	value, canonical, err := decodeCanonicalJSON(raw)
	if err != nil {
		return target, "", err
	}
	if !bytes.Equal(raw, canonical) {
		return target, "", fmt.Errorf("target_ref is not canonical MCE-MANIFEST-JSON1")
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&target); err != nil {
		return target, "", err
	}
	if err := requireUITargetFields(value, target.Kind); err != nil {
		return target, "", err
	}
	if err := validateUITypedRef(target); err != nil {
		return target, "", err
	}
	if target.WorktreeID != worktree || target.ProfileID != profile || target.GenerationID != generation {
		return target, "", fmt.Errorf("target_ref crosses the bound source scope")
	}
	switch target.Kind {
	case uiTargetSymbol:
		return target, string(canonical), requireUITargetRow(ctx, tx, `SELECT count(*) FROM symbols WHERE generation_id=? AND symbol_id=?`, string(generation), string(target.SymbolID))
	case uiTargetEntity:
		return target, string(canonical), requireUITargetRow(ctx, tx, `SELECT count(*) FROM domain_entities WHERE generation_id=? AND entity_id=?`, string(generation), string(target.EntityID))
	case uiTargetModule:
		return target, string(canonical), requireUITargetRow(ctx, tx, `SELECT count(*) FROM modules WHERE generation_id=? AND module_id=?`, string(generation), string(target.ModuleID))
	case uiTargetFile:
		var hash sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT content_hash FROM files WHERE generation_id=? AND path=?`, string(generation), target.Path).Scan(&hash)
		if err != nil {
			return target, "", fmt.Errorf("target file is unavailable: %w", err)
		}
		if target.ContentHash == nil {
			if hash.Valid {
				return target, "", fmt.Errorf("target file reference omitted its captured content hash")
			}
		} else if !hash.Valid || hash.String != string(*target.ContentHash) {
			return target, "", fmt.Errorf("target file content hash differs from the bound generation")
		}
		return target, string(canonical), nil
	case uiTargetSourceAnchor:
		if target.BuildMapID != buildMapID || target.BindingID != bindingID {
			return target, "", fmt.Errorf("source-anchor target must use this map and binding")
		}
		if _, exists := anchors[target.AnchorID]; !exists {
			return target, "", fmt.Errorf("source-anchor target is absent from this map")
		}
		return target, string(canonical), nil
	default:
		return target, "", fmt.Errorf("unsupported UI target reference kind")
	}
}

func validateUITypedRef(target uiTargetRef) error {
	if _, err := model.ParseUUID(string(target.WorktreeID)); err != nil {
		return fmt.Errorf("invalid target worktree_id: %w", err)
	}
	if _, err := model.ParseHash(string(target.ProfileID)); err != nil {
		return fmt.Errorf("invalid target profile_id: %w", err)
	}
	if _, err := model.ParseUUID(string(target.GenerationID)); err != nil {
		return fmt.Errorf("invalid target generation_id: %w", err)
	}
	validIdentifier := func(value model.Identifier) bool {
		return len(value) >= 1 && len(value) <= 192 && utf8.ValidString(string(value))
	}
	switch target.Kind {
	case uiTargetSymbol:
		if !validIdentifier(target.SymbolID) {
			return fmt.Errorf("symbol target requires a 1..192 byte UTF-8 symbol_id")
		}
	case uiTargetEntity:
		if !validIdentifier(target.EntityID) {
			return fmt.Errorf("entity target requires a 1..192 byte UTF-8 entity_id")
		}
	case uiTargetModule:
		if !validIdentifier(target.ModuleID) {
			return fmt.Errorf("module target requires a 1..192 byte UTF-8 module_id")
		}
	case uiTargetSourceAnchor:
		for name, value := range map[string]model.Hash{
			"build_map_id": target.BuildMapID,
			"anchor_id":    target.AnchorID,
			"binding_id":   target.BindingID,
		} {
			if _, err := model.ParseHash(string(value)); err != nil {
				return fmt.Errorf("invalid target %s: %w", name, err)
			}
		}
	case uiTargetFile:
		if err := model.ValidateRelativePath(target.Path); err != nil {
			return fmt.Errorf("invalid target path: %w", err)
		}
		if target.ContentHash != nil {
			if _, err := model.ParseHash(string(*target.ContentHash)); err != nil {
				return fmt.Errorf("invalid target content_hash: %w", err)
			}
		}
	default:
		return fmt.Errorf("unsupported GraphRef kind %q", target.Kind)
	}
	return nil
}

func requireUITargetRow(ctx context.Context, tx *writeTx, query string, args ...any) error {
	var count int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("typed target does not exist in the selected generation")
	}
	return nil
}

func requireUITargetFields(value any, kind uiTargetKind) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("target_ref must be an object")
	}
	keys := []string{"kind", "worktree_id", "profile_id", "generation_id"}
	switch kind {
	case uiTargetSymbol:
		keys = append(keys, "symbol_id")
	case uiTargetEntity:
		keys = append(keys, "entity_id")
	case uiTargetModule:
		keys = append(keys, "module_id")
	case uiTargetFile:
		keys = append(keys, "path", "content_hash")
	case uiTargetSourceAnchor:
		keys = append(keys, "build_map_id", "anchor_id", "binding_id")
	default:
		return fmt.Errorf("unsupported GraphRef kind %q", kind)
	}
	if len(object) != len(keys) {
		return fmt.Errorf("target_ref fields do not match its typed kind")
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return fmt.Errorf("target_ref is missing %q", key)
		}
	}
	return nil
}

func insertOrVerifyUIMap(ctx context.Context, tx *writeTx, manifest UIBuildManifest, metadata []byte, artifactHash model.Hash, createdAt string) (bool, error) {
	var schemaVersion int
	var profile, providerVersion, providerDigest, buildConfig, inputDigest, mode, storedArtifact, storedMetadata string
	err := tx.QueryRowContext(ctx, `SELECT schema_version,profile_id,provider_version,provider_digest,build_config_digest,input_manifest_digest,mode,artifact_hash,metadata_json FROM ui_build_maps WHERE build_map_id=?`, string(manifest.BuildMapID)).
		Scan(&schemaVersion, &profile, &providerVersion, &providerDigest, &buildConfig, &inputDigest, &mode, &storedArtifact, &storedMetadata)
	if err == nil {
		if schemaVersion != manifest.SchemaVersion || profile != string(manifest.ProfileID) || providerVersion != manifest.ProviderVersion || providerDigest != string(manifest.ProviderDigest) || buildConfig != string(manifest.BuildConfigDigest) || inputDigest != string(manifest.InputManifestDigest) || mode != manifest.Mode || storedArtifact != string(artifactHash) || storedMetadata != string(metadata) {
			return false, ErrUIIntegrityConflict
		}
		return true, verifyUIAnchors(ctx, tx, manifest)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read existing UI build map: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ui_build_maps(build_map_id,schema_version,profile_id,provider_version,provider_digest,build_config_digest,input_manifest_digest,mode,artifact_hash,metadata_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		string(manifest.BuildMapID), manifest.SchemaVersion, string(manifest.ProfileID), manifest.ProviderVersion, string(manifest.ProviderDigest), string(manifest.BuildConfigDigest), string(manifest.InputManifestDigest), manifest.Mode, string(artifactHash), string(metadata), createdAt); err != nil {
		return false, fmt.Errorf("insert immutable UI build map: %w", err)
	}
	for _, anchor := range manifest.Anchors {
		var owner any
		if anchor.OwnerAnchorID != nil {
			owner = string(*anchor.OwnerAnchorID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ui_source_anchors(build_map_id,anchor_id,kind,path,source_hash,start_byte,end_byte,owner_anchor_id) VALUES(?,?,?,?,?,?,?,?)`,
			string(manifest.BuildMapID), string(anchor.AnchorID), anchor.Kind, anchor.Path, string(anchor.ContentHash), anchor.Range.StartByte, anchor.Range.EndByte, owner); err != nil {
			return false, fmt.Errorf("insert immutable UI source anchor: %w", err)
		}
	}
	return false, nil
}

func verifyUIAnchors(ctx context.Context, tx *writeTx, manifest UIBuildManifest) error {
	anchors := append([]UIManifestAnchor(nil), manifest.Anchors...)
	sort.Slice(anchors, func(i, j int) bool { return anchors[i].AnchorID < anchors[j].AnchorID })
	expected := make([]any, 0, len(anchors))
	for _, want := range anchors {
		var wantOwner string
		if want.OwnerAnchorID != nil {
			wantOwner = string(*want.OwnerAnchorID)
		}
		var owner any
		if want.OwnerAnchorID != nil {
			owner = wantOwner
		}
		expected = append(expected, map[string]any{
			"anchor_id": want.AnchorID, "kind": want.Kind, "path": want.Path, "source_hash": want.ContentHash,
			"start_byte": want.Range.StartByte, "end_byte": want.Range.EndByte, "owner_anchor_id": owner,
		})
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_group_array(json_object('anchor_id',anchor_id,'kind',kind,'path',path,'source_hash',source_hash,'start_byte',start_byte,'end_byte',end_byte,'owner_anchor_id',owner_anchor_id)),'[]') FROM (SELECT anchor_id,kind,path,source_hash,start_byte,end_byte,owner_anchor_id FROM ui_source_anchors WHERE build_map_id=? ORDER BY anchor_id)`, string(manifest.BuildMapID)).Scan(&stored); err != nil {
		return err
	}
	want, err := model.CanonicalJSON1(expected)
	if err != nil {
		return err
	}
	_, got, err := decodeCanonicalJSON([]byte(stored))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return ErrUIIntegrityConflict
	}
	return nil
}

func insertOrVerifyUIBinding(ctx context.Context, tx *writeTx, manifest UIBuildManifest, request UIBindingRequest, bindingID, evidenceDigest model.Hash, createdAt string, prepared preparedUILinks) (bool, error) {
	var existingMap, profile, sourceGeneration, policy, resolver, storedEvidence string
	err := tx.QueryRowContext(ctx, `SELECT build_map_id,profile_id,source_generation_id,policy_digest,resolver_digest,evidence_digest FROM ui_build_bindings WHERE binding_id=?`, string(bindingID)).
		Scan(&existingMap, &profile, &sourceGeneration, &policy, &resolver, &storedEvidence)
	if errors.Is(err, sql.ErrNoRows) {
		var naturalCount int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ui_build_bindings WHERE build_map_id=? AND profile_id=? AND source_generation_id=? AND policy_digest=? AND resolver_digest=?`,
			string(manifest.BuildMapID), string(request.ProfileID), string(request.SourceGenerationID), string(request.PolicyDigest), string(request.ResolverDigest)).Scan(&naturalCount); err != nil {
			return false, err
		}
		if naturalCount != 0 {
			return false, ErrUIIntegrityConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ui_build_bindings(binding_id,build_map_id,profile_id,source_generation_id,policy_digest,resolver_digest,evidence_digest,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			string(bindingID), string(manifest.BuildMapID), string(request.ProfileID), string(request.SourceGenerationID), string(request.PolicyDigest), string(request.ResolverDigest), string(evidenceDigest), createdAt); err != nil {
			return false, fmt.Errorf("insert immutable UI binding: %w", err)
		}
		for _, link := range prepared.links {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ui_anchor_links(binding_id,build_map_id,link_id,anchor_id,target_ref_json,kind,resolution_status,evidence_json) VALUES(?,?,?,?,?,?,?,?)`,
				string(bindingID), string(manifest.BuildMapID), link.linkID, string(link.anchorID), link.targetJSON, link.kind, link.status, link.evidenceJSON); err != nil {
				return false, fmt.Errorf("insert immutable UI anchor link: %w", err)
			}
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read existing UI binding: %w", err)
	}
	if existingMap != string(manifest.BuildMapID) || profile != string(request.ProfileID) || sourceGeneration != string(request.SourceGenerationID) || policy != string(request.PolicyDigest) || resolver != string(request.ResolverDigest) || storedEvidence != string(evidenceDigest) {
		return false, ErrUIIntegrityConflict
	}
	expectedLinks := make([]any, 0, len(prepared.links))
	for _, link := range prepared.links {
		target, _, err := decodeCanonicalJSON([]byte(link.targetJSON))
		if err != nil {
			return false, err
		}
		evidence, _, err := decodeCanonicalJSON([]byte(link.evidenceJSON))
		if err != nil {
			return false, err
		}
		expectedLinks = append(expectedLinks, map[string]any{
			"link_id": link.linkID, "anchor_id": link.anchorID, "target_ref": target,
			"kind": link.kind, "resolution_status": link.status, "evidence": evidence,
		})
	}
	var storedLinks string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_group_array(json_object('link_id',link_id,'anchor_id',anchor_id,'target_ref',json(target_ref_json),'kind',kind,'resolution_status',resolution_status,'evidence',json(evidence_json))),'[]') FROM (SELECT link_id,anchor_id,target_ref_json,kind,resolution_status,evidence_json FROM ui_anchor_links WHERE binding_id=? ORDER BY link_id)`, string(bindingID)).Scan(&storedLinks); err != nil {
		return false, err
	}
	wantLinks, err := model.CanonicalJSON1(expectedLinks)
	if err != nil {
		return false, err
	}
	_, gotLinks, err := decodeCanonicalJSON([]byte(storedLinks))
	if err != nil {
		return false, err
	}
	if !bytes.Equal(gotLinks, wantLinks) {
		return false, ErrUIIntegrityConflict
	}
	return true, nil
}

func decodeUIManifest(raw []byte) (UIBuildManifest, []byte, error) {
	var empty UIBuildManifest
	if len(raw) == 0 || len(raw) > maxUIManifestBytes || !utf8.Valid(raw) {
		return empty, nil, fmt.Errorf("UI manifest must be valid UTF-8 and at most %d bytes: %w", maxUIManifestBytes, ErrInvalidIndex)
	}
	if err := validateUISurrogateEscapes(raw); err != nil {
		return empty, nil, fmt.Errorf("invalid UI manifest Unicode: %w", err)
	}
	value, canonical, err := decodeCanonicalJSON(raw)
	if err != nil {
		return empty, nil, fmt.Errorf("decode UI manifest: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return empty, nil, fmt.Errorf("UI manifest is not canonical MCE-MANIFEST-JSON1: %w", ErrInvalidIndex)
	}
	if err := validateUIManifestShape(value); err != nil {
		return empty, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&empty); err != nil {
		return empty, nil, fmt.Errorf("decode closed UI manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return empty, nil, err
	}
	if err := normalizeAndValidateUIManifest(&empty); err != nil {
		return empty, nil, err
	}
	normalized, err := model.CanonicalJSON1(uiManifestValue(empty))
	if err != nil {
		return empty, nil, err
	}
	if !bytes.Equal(raw, normalized) {
		return empty, nil, fmt.Errorf("UI manifest arrays are not in canonical order: %w", ErrInvalidIndex)
	}
	return empty, normalized, nil
}

func decodeCanonicalJSON(raw []byte) (any, []byte, error) {
	if len(raw) == 0 || len(raw) > maxUIManifestBytes || !utf8.Valid(raw) {
		return nil, nil, fmt.Errorf("UI JSON size or UTF-8 is invalid")
	}
	if err := validateUISurrogateEscapes(raw); err != nil {
		return nil, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	value, err := decodeUIJSONValue(decoder, 0, &nodes)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, nil, err
	}
	canonical, err := model.CanonicalJSON1(value)
	if err != nil {
		return nil, nil, err
	}
	return value, canonical, nil
}

func decodeUIJSONValue(decoder *json.Decoder, depth int, nodes *int) (any, error) {
	*nodes++
	if *nodes > maxUIJSONNodes || depth > 32 {
		return nil, fmt.Errorf("UI JSON node or depth limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("UI JSON object key is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate UI JSON member %q", key)
			}
			if len(object) >= 4096 {
				return nil, fmt.Errorf("UI JSON object key limit exceeded")
			}
			value, err := decodeUIJSONValue(decoder, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated UI JSON object")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			if len(array) >= maxUIEntries {
				return nil, fmt.Errorf("UI JSON array limit exceeded")
			}
			value, err := decodeUIJSONValue(decoder, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated UI JSON array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected UI JSON delimiter %q", delim)
	}
}

func ensureJSONEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("UI JSON has trailing input")
	}
	return nil
}

func validateUIManifestShape(value any) error {
	root, ok := value.(map[string]any)
	if !ok || !hasExactUIKeys(root, "schema_version", "worktree_id", "profile_id", "build_map_id", "provider_version", "build_config_digest", "input_manifest_digest", "mode", "module_coverage", "sources", "anchors", "provider_digest") {
		return fmt.Errorf("UI manifest fields do not match the closed BuildManifest schema: %w", ErrInvalidIndex)
	}
	sources, ok := root["sources"].([]any)
	if !ok || len(sources) > maxUIEntries {
		return fmt.Errorf("UI manifest sources are invalid or oversized: %w", ErrInvalidIndex)
	}
	for _, item := range sources {
		object, ok := item.(map[string]any)
		if !ok || !hasExactUIKeys(object, "path", "content_hash") {
			return fmt.Errorf("UI manifest source has invalid fields: %w", ErrInvalidIndex)
		}
	}
	anchors, ok := root["anchors"].([]any)
	if !ok || len(anchors) > maxUIEntries {
		return fmt.Errorf("UI manifest anchors are invalid or oversized: %w", ErrInvalidIndex)
	}
	for _, item := range anchors {
		object, ok := item.(map[string]any)
		if !ok || !hasExactUIKeys(object, "anchor_id", "kind", "path", "content_hash", "range", "owner_anchor_id") {
			return fmt.Errorf("UI manifest anchor has invalid fields: %w", ErrInvalidIndex)
		}
		rangeObject, ok := object["range"].(map[string]any)
		if !ok || !hasExactUIKeys(rangeObject, "start_byte", "end_byte") {
			return fmt.Errorf("UI manifest anchor range has invalid fields: %w", ErrInvalidIndex)
		}
	}
	return nil
}

func hasExactUIKeys(object map[string]any, keys ...string) bool {
	if len(object) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return false
		}
	}
	return true
}

func normalizeAndValidateUIManifest(manifest *UIBuildManifest) error {
	if manifest.SchemaVersion != 1 || len(manifest.Sources) > maxUIEntries || len(manifest.Anchors) > maxUIEntries {
		return fmt.Errorf("UI manifest version or collection size is invalid: %w", ErrInvalidIndex)
	}
	if _, err := model.ParseUUID(string(manifest.WorktreeID)); err != nil {
		return err
	}
	for name, digest := range map[string]model.Hash{"profile_id": manifest.ProfileID, "build_map_id": manifest.BuildMapID, "provider_digest": manifest.ProviderDigest, "build_config_digest": manifest.BuildConfigDigest, "input_manifest_digest": manifest.InputManifestDigest} {
		if _, err := model.ParseHash(string(digest)); err != nil {
			return fmt.Errorf("invalid %s: %w", name, err)
		}
	}
	if !utf8.ValidString(manifest.ProviderVersion) || utf8.RuneCountInString(manifest.ProviderVersion) < 1 || utf8.RuneCountInString(manifest.ProviderVersion) > 128 {
		return fmt.Errorf("UI provider_version must contain 1..128 Unicode code points: %w", ErrInvalidIndex)
	}
	if manifest.Mode != "development" && manifest.Mode != "test" {
		return fmt.Errorf("UI BuildManifest mode is not development/test: %w", ErrInvalidIndex)
	}
	if manifest.ModuleCoverage != "all_declared_modules" && manifest.ModuleCoverage != "loaded_modules_only" && manifest.ModuleCoverage != "partial" {
		return fmt.Errorf("UI module_coverage is unsupported: %w", ErrInvalidIndex)
	}
	sort.Slice(manifest.Sources, func(i, j int) bool {
		return bytes.Compare([]byte(manifest.Sources[i].Path), []byte(manifest.Sources[j].Path)) < 0
	})
	for index, source := range manifest.Sources {
		if err := validateUIPath(source.Path); err != nil {
			return err
		}
		if _, err := model.ParseHash(string(source.ContentHash)); err != nil {
			return err
		}
		if index > 0 && manifest.Sources[index-1].Path == source.Path {
			return fmt.Errorf("UI manifest has duplicate source paths: %w", ErrInvalidIndex)
		}
	}
	sourceHashes := make(map[string]model.Hash, len(manifest.Sources))
	for _, source := range manifest.Sources {
		sourceHashes[source.Path] = source.ContentHash
	}
	sort.Slice(manifest.Anchors, func(i, j int) bool { return compareUIAnchors(manifest.Anchors[i], manifest.Anchors[j]) < 0 })
	anchorIDs := make(map[model.Hash]struct{}, len(manifest.Anchors))
	for index := range manifest.Anchors {
		anchor := &manifest.Anchors[index]
		if _, err := model.ParseHash(string(anchor.AnchorID)); err != nil {
			return err
		}
		if _, err := model.ParseHash(string(anchor.ContentHash)); err != nil {
			return err
		}
		if err := validateUIPath(anchor.Path); err != nil {
			return err
		}
		if anchor.Kind != "element_site" && anchor.Kind != "component_definition" && anchor.Kind != "component_render_site" && anchor.Kind != "event_binding_site" {
			return fmt.Errorf("unsupported UI anchor kind %q: %w", anchor.Kind, ErrInvalidIndex)
		}
		if anchor.Range.StartByte < 0 || anchor.Range.EndByte < anchor.Range.StartByte || anchor.Range.EndByte > model.MaxSafeInteger {
			return fmt.Errorf("invalid UI anchor byte range: %w", ErrInvalidIndex)
		}
		hash, exists := sourceHashes[anchor.Path]
		if !exists || hash != anchor.ContentHash {
			return fmt.Errorf("UI anchor does not match a manifest source: %w", ErrInvalidIndex)
		}
		if _, exists := anchorIDs[anchor.AnchorID]; exists {
			return fmt.Errorf("UI manifest has duplicate anchor IDs: %w", ErrInvalidIndex)
		}
		anchorIDs[anchor.AnchorID] = struct{}{}
		expected, err := model.HashValue("anchor-v1", map[string]any{
			"path": anchor.Path, "content_hash": anchor.ContentHash, "kind": anchor.Kind,
			"start_byte": anchor.Range.StartByte, "end_byte": anchor.Range.EndByte, "provider_digest": manifest.ProviderDigest,
		})
		if err != nil || expected != anchor.AnchorID {
			return fmt.Errorf("UI source anchor ID does not match C14N1 recipe: %w", ErrInvalidIndex)
		}
	}
	for _, anchor := range manifest.Anchors {
		if anchor.OwnerAnchorID == nil {
			continue
		}
		owner, exists := findUIAnchor(manifest.Anchors, *anchor.OwnerAnchorID)
		if !exists || owner.Kind != "component_definition" || owner.Path != anchor.Path || owner.AnchorID == anchor.AnchorID {
			return fmt.Errorf("UI anchor owner is absent, incompatible, or cross-file: %w", ErrInvalidIndex)
		}
	}
	if hasUIOwnerCycle(manifest.Anchors) {
		return fmt.Errorf("UI anchor ownership contains a cycle: %w", ErrInvalidIndex)
	}
	computed, err := model.HashValue("build-map-v1", uiBuildMapValue(*manifest))
	if err != nil || computed != manifest.BuildMapID {
		return fmt.Errorf("UI build_map_id does not match C14N1 recipe: %w", ErrInvalidIndex)
	}
	return nil
}

func validateUIPath(value string) error {
	if utf8.RuneCountInString(value) > 4096 {
		return fmt.Errorf("UI source path exceeds the contract limit: %w", ErrInvalidIndex)
	}
	return model.ValidateRelativePath(value)
}

func compareUIAnchors(left, right UIManifestAnchor) int {
	if cmp := bytes.Compare([]byte(left.Path), []byte(right.Path)); cmp != 0 {
		return cmp
	}
	if left.Range.StartByte < right.Range.StartByte {
		return -1
	}
	if left.Range.StartByte > right.Range.StartByte {
		return 1
	}
	if left.Range.EndByte < right.Range.EndByte {
		return -1
	}
	if left.Range.EndByte > right.Range.EndByte {
		return 1
	}
	if left.Kind < right.Kind {
		return -1
	}
	if left.Kind > right.Kind {
		return 1
	}
	return strings.Compare(string(left.AnchorID), string(right.AnchorID))
}

func findUIAnchor(anchors []UIManifestAnchor, id model.Hash) (UIManifestAnchor, bool) {
	for _, anchor := range anchors {
		if anchor.AnchorID == id {
			return anchor, true
		}
	}
	return UIManifestAnchor{}, false
}

func hasUIOwnerCycle(anchors []UIManifestAnchor) bool {
	owners := make(map[model.Hash]model.Hash, len(anchors))
	for _, anchor := range anchors {
		if anchor.OwnerAnchorID != nil {
			owners[anchor.AnchorID] = *anchor.OwnerAnchorID
		}
	}
	for id := range owners {
		seen := make(map[model.Hash]struct{})
		for current := id; current != ""; current = owners[current] {
			if _, exists := seen[current]; exists {
				return true
			}
			seen[current] = struct{}{}
		}
	}
	return false
}

func uiBuildMapValue(manifest UIBuildManifest) map[string]any {
	sources := make([]any, 0, len(manifest.Sources))
	for _, source := range manifest.Sources {
		sources = append(sources, map[string]any{"path": source.Path, "content_hash": source.ContentHash})
	}
	anchors := make([]any, 0, len(manifest.Anchors))
	for _, anchor := range manifest.Anchors {
		var owner any
		if anchor.OwnerAnchorID != nil {
			owner = *anchor.OwnerAnchorID
		}
		anchors = append(anchors, map[string]any{
			"anchor_id": anchor.AnchorID, "kind": anchor.Kind, "path": anchor.Path,
			"content_hash":    anchor.ContentHash,
			"range":           map[string]any{"start_byte": anchor.Range.StartByte, "end_byte": anchor.Range.EndByte},
			"owner_anchor_id": owner,
		})
	}
	return map[string]any{
		"schema_version": manifest.SchemaVersion, "worktree_id": manifest.WorktreeID, "profile_id": manifest.ProfileID,
		"provider_version": manifest.ProviderVersion, "build_config_digest": manifest.BuildConfigDigest,
		"input_manifest_digest": manifest.InputManifestDigest, "mode": manifest.Mode, "module_coverage": manifest.ModuleCoverage,
		"sources": sources, "anchors": anchors, "provider_digest": manifest.ProviderDigest,
	}
}

func uiManifestValue(manifest UIBuildManifest) map[string]any {
	value := uiBuildMapValue(manifest)
	value["build_map_id"] = manifest.BuildMapID
	return value
}

func canonicalSourceRefs(refs []model.SourceRef) ([]byte, error) {
	return model.CanonicalJSON1(sourceRefValues(refs))
}

func sourceRefValues(refs []model.SourceRef) []any {
	values := make([]any, 0, len(refs))
	for _, ref := range refs {
		values = append(values, map[string]any{
			"worktree_id": ref.WorktreeID, "profile_id": ref.ProfileID, "generation_id": ref.GenerationID,
			"path": ref.Path, "content_hash": ref.ContentHash,
			"range": map[string]any{"start_byte": ref.Range.StartByte, "end_byte": ref.Range.EndByte},
		})
	}
	return values
}

func uiImportPeakGrowth(manifestBytes, anchors, links int) int64 {
	// The reservation is deliberately conservative: artifact bytes plus bounded
	// row/index overhead for every manifest anchor and binding link.
	peak := int64(manifestBytes) + int64(anchors)*1024 + int64(links)*2048 + 1<<20
	if peak > model.MaxSafeInteger {
		return model.MaxSafeInteger
	}
	return peak
}

func validUIIdentifier(value string) bool {
	if value == "" || len(value) > 192 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validateUISurrogateEscapes(raw []byte) error {
	for index := 0; index < len(raw); {
		if raw[index] != '"' {
			index++
			continue
		}
		index++
		for index < len(raw) && raw[index] != '"' {
			if raw[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(raw) {
				return fmt.Errorf("truncated JSON escape")
			}
			if raw[index+1] != 'u' {
				index += 2
				continue
			}
			if index+6 > len(raw) {
				return fmt.Errorf("truncated Unicode escape")
			}
			code, err := strconv.ParseUint(string(raw[index+2:index+6]), 16, 16)
			if err != nil {
				return fmt.Errorf("invalid Unicode escape")
			}
			index += 6
			if code >= 0xD800 && code <= 0xDBFF {
				if index+6 > len(raw) || raw[index] != '\\' || raw[index+1] != 'u' {
					return fmt.Errorf("unpaired high surrogate")
				}
				low, err := strconv.ParseUint(string(raw[index+2:index+6]), 16, 16)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return fmt.Errorf("invalid low surrogate")
				}
				index += 6
			} else if code >= 0xDC00 && code <= 0xDFFF {
				return fmt.Errorf("unpaired low surrogate")
			}
		}
		if index >= len(raw) {
			return fmt.Errorf("unterminated JSON string")
		}
		index++
	}
	return nil
}
