package model

import (
	"bytes"
	"fmt"
	"sort"
	"unicode/utf8"
)

func (vector ScopeVector) Validate() error {
	if vector.Kind != "single" && vector.Kind != "federated" {
		return fmt.Errorf("%w: invalid scope-vector kind", ErrInvalidValue)
	}
	if vector.Members == nil || vector.UnavailableMembers == nil || len(vector.Members) > 32 || len(vector.UnavailableMembers) > 32 {
		return fmt.Errorf("%w: scope-vector member arrays are required and capped at 32", ErrInvalidValue)
	}
	if _, err := ParseHash(string(vector.VectorDigest)); err != nil {
		return err
	}
	if _, err := ParseUUID(string(vector.CatalogIncarnation)); err != nil {
		return err
	}
	if vector.Kind == "single" {
		if len(vector.Members) != 1 || len(vector.UnavailableMembers) != 0 || vector.GroupID != nil || vector.GroupRevision != nil {
			return fmt.Errorf("%w: single scope needs one resolved member and no group fields", ErrInvalidValue)
		}
	} else {
		if vector.GroupID == nil || vector.GroupRevision == nil || *vector.GroupRevision < 1 || *vector.GroupRevision > MaxSafeInteger {
			return fmt.Errorf("%w: federated scope needs a pinned group revision", ErrInvalidValue)
		}
		if _, err := ParseUUID(string(*vector.GroupID)); err != nil {
			return err
		}
	}
	for _, member := range vector.Members {
		if err := validateScopeMember(member); err != nil {
			return err
		}
	}
	for _, member := range vector.UnavailableMembers {
		if err := validateUnavailableMember(member); err != nil {
			return err
		}
	}
	computed, err := vector.ComputeDigest()
	if err != nil {
		return err
	}
	if computed != vector.VectorDigest {
		return fmt.Errorf("%w: scope vector digest mismatch", ErrInvalidValue)
	}
	return nil
}

func (vector ScopeVector) ComputeDigest() (Hash, error) {
	members := append([]ScopeMember(nil), vector.Members...)
	sort.Slice(members, func(i, j int) bool {
		left, right := members[i], members[j]
		if cmp := compareOptional(left.ServiceID, right.ServiceID); cmp != 0 {
			return cmp < 0
		}
		if cmp := compareString(string(left.WorktreeID), string(right.WorktreeID)); cmp != 0 {
			return cmp < 0
		}
		if cmp := compareString(string(left.ProfileID), string(right.ProfileID)); cmp != 0 {
			return cmp < 0
		}
		return compareString(string(left.GenerationID), string(right.GenerationID)) < 0
	})
	for i := 1; i < len(members); i++ {
		if compareScopeMemberKey(members[i-1], members[i]) == 0 {
			return "", fmt.Errorf("%w: duplicate scope-vector member identity", ErrInvalidValue)
		}
	}

	unavailable := append([]UnavailableMember(nil), vector.UnavailableMembers...)
	sort.Slice(unavailable, func(i, j int) bool {
		left, right := unavailable[i], unavailable[j]
		if cmp := compareOptional(left.ServiceID, right.ServiceID); cmp != 0 {
			return cmp < 0
		}
		if cmp := compareOptionalUUID(left.WorktreeID, right.WorktreeID); cmp != 0 {
			return cmp < 0
		}
		return compareString(left.Status, right.Status) < 0
	})
	for i := 1; i < len(unavailable); i++ {
		if compareUnavailableKey(unavailable[i-1], unavailable[i]) == 0 {
			return "", fmt.Errorf("%w: duplicate unavailable-member identity", ErrInvalidValue)
		}
	}

	memberValues := make([]any, 0, len(members))
	for _, member := range members {
		memberValues = append(memberValues, map[string]any{
			"worktree_id":         member.WorktreeID,
			"profile_id":          member.ProfileID,
			"generation_id":       member.GenerationID,
			"snapshot_digest":     member.SnapshotDigest,
			"policy_digest":       member.PolicyDigest,
			"policy_epoch":        member.PolicyEpoch,
			"binding_revision":    member.BindingRevision,
			"record_epoch":        member.RecordEpoch,
			"service_id":          optionalString(member.ServiceID),
			"index_incarnation":   member.IndexIncarnation,
			"records_incarnation": member.RecordsIncarnation,
		})
	}
	unavailableValues := make([]any, 0, len(unavailable))
	for _, member := range unavailable {
		unavailableValues = append(unavailableValues, map[string]any{
			"service_id":  optionalString(member.ServiceID),
			"worktree_id": optionalUUID(member.WorktreeID),
			"profile_id":  optionalHash(member.ProfileID),
			"status":      member.Status,
			"reason_code": member.ReasonCode,
		})
	}
	value := map[string]any{
		"kind":                vector.Kind,
		"members":             memberValues,
		"group_id":            optionalUUID(vector.GroupID),
		"group_revision":      optionalInt(vector.GroupRevision),
		"catalog_incarnation": vector.CatalogIncarnation,
		"unavailable_members": unavailableValues,
	}
	return HashValue("scope-v1", value)
}

func validateScopeMember(member ScopeMember) error {
	for _, value := range []UUID{member.WorktreeID, member.GenerationID, member.IndexIncarnation, member.RecordsIncarnation} {
		if _, err := ParseUUID(string(value)); err != nil {
			return err
		}
	}
	for _, value := range []Hash{member.ProfileID, member.SnapshotDigest, member.PolicyDigest} {
		if _, err := ParseHash(string(value)); err != nil {
			return err
		}
	}
	if member.PolicyEpoch < 1 || member.BindingRevision < 1 || member.RecordEpoch < 0 || member.PolicyEpoch > MaxSafeInteger || member.BindingRevision > MaxSafeInteger || member.RecordEpoch > MaxSafeInteger {
		return fmt.Errorf("%w: invalid scope member epoch", ErrInvalidValue)
	}
	if member.ServiceID != nil && !validIdentifier(*member.ServiceID) {
		return fmt.Errorf("%w: invalid service ID", ErrInvalidValue)
	}
	return nil
}

func validateUnavailableMember(member UnavailableMember) error {
	if member.ServiceID != nil && !validIdentifier(*member.ServiceID) {
		return fmt.Errorf("%w: invalid service ID", ErrInvalidValue)
	}
	if member.WorktreeID != nil {
		if _, err := ParseUUID(string(*member.WorktreeID)); err != nil {
			return err
		}
	}
	if member.ProfileID != nil {
		if _, err := ParseHash(string(*member.ProfileID)); err != nil {
			return err
		}
	}
	if !oneOf(member.Status, "index_missing", "unavailable", "access_denied", "profile_missing") || !validIdentifier(member.ReasonCode) {
		return fmt.Errorf("%w: unavailable member needs a known status and valid reason", ErrInvalidValue)
	}
	return nil
}

func validIdentifier(value Identifier) bool {
	return len(value) >= 1 && len(value) <= 192 && utf8.ValidString(string(value))
}

func compareScopeMemberKey(left, right ScopeMember) int {
	if cmp := compareOptional(left.ServiceID, right.ServiceID); cmp != 0 {
		return cmp
	}
	for _, pair := range [][2]string{
		{string(left.WorktreeID), string(right.WorktreeID)},
		{string(left.ProfileID), string(right.ProfileID)},
		{string(left.GenerationID), string(right.GenerationID)},
	} {
		if cmp := compareString(pair[0], pair[1]); cmp != 0 {
			return cmp
		}
	}
	return 0
}

func compareUnavailableKey(left, right UnavailableMember) int {
	if cmp := compareOptional(left.ServiceID, right.ServiceID); cmp != 0 {
		return cmp
	}
	if cmp := compareOptionalUUID(left.WorktreeID, right.WorktreeID); cmp != 0 {
		return cmp
	}
	return compareString(left.Status, right.Status)
}

func compareOptional(left, right *Identifier) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	return compareString(string(*left), string(*right))
}

func compareOptionalUUID(left, right *UUID) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	return compareString(string(*left), string(*right))
}

func compareString(left, right string) int {
	return bytes.Compare([]byte(left), []byte(right))
}

func optionalString(value *Identifier) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalUUID(value *UUID) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalHash(value *Hash) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func (manifest CaptureManifest) Validate() error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("%w: unsupported capture manifest schema version", ErrInvalidValue)
	}
	if manifest.Files == nil || manifest.DependencyInputDigests == nil || len(manifest.Files) > 1_000_000 || len(manifest.DependencyInputDigests) > 1_000_000 {
		return fmt.Errorf("%w: capture manifest arrays are required and capped at 1000000", ErrInvalidValue)
	}
	if _, err := ParseUUID(string(manifest.WorktreeID)); err != nil {
		return err
	}
	for _, value := range []Hash{manifest.ProfileID, manifest.PolicyDigest, manifest.ProviderDigest, manifest.SnapshotDigest} {
		if _, err := ParseHash(string(value)); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		if err := ValidateRelativePath(file.Path); err != nil {
			return err
		}
		if !oneOf(file.Status, "captured", "metadata_only", "excluded", "missing", "read_error") {
			return fmt.Errorf("%w: unknown capture-manifest file status", ErrInvalidValue)
		}
		if _, exists := seen[file.Path]; exists {
			return fmt.Errorf("%w: duplicate capture-manifest path", ErrInvalidValue)
		}
		seen[file.Path] = struct{}{}
		if file.ContentHash != nil {
			if _, err := ParseHash(string(*file.ContentHash)); err != nil {
				return err
			}
		}
		if file.ByteLength != nil && (*file.ByteLength < 0 || *file.ByteLength > MaxSafeInteger) {
			return fmt.Errorf("%w: capture-manifest byte length outside safe range", ErrInvalidValue)
		}
		if file.ReasonCode != nil && !validIdentifier(*file.ReasonCode) {
			return fmt.Errorf("%w: invalid capture-manifest reason code", ErrInvalidValue)
		}
		if file.Status == "captured" {
			if file.ContentHash == nil || file.ByteLength == nil {
				return fmt.Errorf("%w: captured file needs hash and safe byte length", ErrInvalidValue)
			}
		}
	}
	for _, digest := range manifest.DependencyInputDigests {
		if _, err := ParseHash(string(digest)); err != nil {
			return err
		}
	}
	computed, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		return err
	}
	if computed != manifest.SnapshotDigest {
		return fmt.Errorf("%w: capture snapshot digest mismatch", ErrInvalidValue)
	}
	return nil
}

func (manifest CaptureManifest) ComputeSnapshotDigest() (Hash, error) {
	files := append([]ManifestFile(nil), manifest.Files...)
	sort.Slice(files, func(i, j int) bool { return compareString(files[i].Path, files[j].Path) < 0 })
	fileValues := make([]any, 0, len(files))
	for _, file := range files {
		fileValues = append(fileValues, map[string]any{
			"path":         file.Path,
			"status":       file.Status,
			"content_hash": optionalHash(file.ContentHash),
			"byte_length":  optionalInt(file.ByteLength),
			"reason_code":  optionalIdentifier(file.ReasonCode),
		})
	}
	dependencies := append([]Hash(nil), manifest.DependencyInputDigests...)
	sort.Slice(dependencies, func(i, j int) bool { return compareString(string(dependencies[i]), string(dependencies[j])) < 0 })
	dependencyValues := make([]any, 0, len(dependencies))
	for _, digest := range dependencies {
		dependencyValues = append(dependencyValues, digest)
	}
	return HashValue("capture-v1", map[string]any{
		"schema_version":           manifest.SchemaVersion,
		"worktree_id":              manifest.WorktreeID,
		"profile_id":               manifest.ProfileID,
		"policy_digest":            manifest.PolicyDigest,
		"provider_digest":          manifest.ProviderDigest,
		"files":                    fileValues,
		"dependency_input_digests": dependencyValues,
	})
}

func optionalIdentifier(value *Identifier) any {
	if value == nil {
		return nil
	}
	return *value
}
