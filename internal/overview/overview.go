// Package overview builds deterministic, source-backed hierarchy summaries.
// It consumes normalized facts and never discovers ownership from folder names.
package overview

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrInvalidInput     = errors.New("invalid overview input")
	ErrUnknownExpandRef = errors.New("unknown overview expansion reference")
)

type Scope struct {
	WorktreeID   model.UUID
	ProfileID    model.Hash
	GenerationID model.UUID
}

type SourceEvidence struct {
	Path        string
	ContentHash model.Hash
	Range       model.ByteRange
}

type SourceFile struct {
	Path        string
	ContentHash model.Hash
	ByteLength  int64
	Language    string
	Role        string
}

type ModuleInput struct {
	Key       string
	Label     string
	Kind      string
	Plane     string
	ParentKey string
	Source    SourceEvidence
}

type ModuleMembershipInput struct {
	ModuleKey string
	Source    SourceEvidence
}

// Memberships are explicit and independently source-backed. Multiple module
// memberships are valid for shared packages.
type PackageInput struct {
	Key               model.Hash
	ImportPath        string
	Variant           string
	ModuleMemberships []ModuleMembershipInput
	Files             []string
	SymbolCount       int
	DiagnosticCount   int
}

type ReferenceInput struct {
	ID               model.Hash
	SourcePackageKey model.Hash
	TargetPackageKey *model.Hash
	Kind             string
	EvidenceKind     string
	ResolutionStatus string
	Source           SourceEvidence
}

type EntryPointInput struct {
	PackageKey model.Hash
	Kind       string
	Name       string
	Source     SourceEvidence
}

type DocumentInput struct {
	Title    string
	TrustRef string
	Source   SourceEvidence
}

type Input struct {
	Scope            Scope
	Files            []SourceFile
	Modules          []ModuleInput
	Packages         []PackageInput
	References       []ReferenceInput
	EntryPoints      []EntryPointInput
	TrustedDocuments []DocumentInput
	KnownGaps        []Gap
}

type Module struct {
	ID          model.Hash
	Key         string
	Label       string
	Kind        string
	Plane       string
	ParentID    *model.Hash
	Source      SourceEvidence
	PackageKeys []model.Hash
	ChildIDs    []model.Hash
	FileCount   int
}

type Package struct {
	Key                    model.Hash
	ImportPath             string
	Variant                string
	ModuleIDs              []model.Hash
	ModuleMemberships      []ModuleMembership
	Files                  []string
	SymbolCount            int
	DiagnosticCount        int
	IncomingReferenceCount int
	OutgoingReferenceCount int
}

type ModuleMembership struct {
	ModuleID model.Hash
	Source   SourceEvidence
}

type Reference struct {
	ID               model.Hash
	SourcePackageKey model.Hash
	TargetPackageKey *model.Hash
	SourceModuleIDs  []model.Hash
	TargetModuleIDs  []model.Hash
	Kind             string
	EvidenceKind     string
	ResolutionStatus string
	Source           SourceEvidence
}

type Dependency struct {
	ID               model.Hash
	FromModuleID     model.Hash
	ToModuleID       model.Hash
	Kind             string
	EvidenceKind     string
	ResolutionStatus string
	ReferenceCount   int
	ExpandRef        model.Hash
}

type EntryPoint struct {
	ID         model.Hash
	PackageKey model.Hash
	Kind       string
	Name       string
	Source     SourceEvidence
}

type TrustedDocument struct {
	Title    string
	TrustRef string
	Source   SourceEvidence
}

type Gap struct {
	Code  string
	Count int
}

type Capabilities struct {
	D1Structure              string
	D2InteractionsAndData    string
	D3BehaviorAndConstraints string
	D4EvolutionAndEvidence   string
}

type Statistics struct {
	FileCount                 int
	PackageCount              int
	ModuleCount               int
	ReferenceCount            int
	GroupedReferenceCount     int
	CatalogOnlyReferenceCount int
	DependencyGroupCount      int
	CapturedBytes             int64
}

type Overview struct {
	WorkspaceID      model.Hash
	Scope            Scope
	Languages        []string
	Modules          []Module
	Packages         []Package
	Dependencies     []Dependency
	ReferenceCatalog []Reference
	EntryPoints      []EntryPoint
	TrustedDocuments []TrustedDocument
	TestRoots        []string
	Gaps             []Gap
	Capabilities     Capabilities
	Statistics       Statistics
	expansions       map[model.Hash][]Reference
}

type dependencyKey struct {
	from, to, kind, evidence, status string
}

// Build validates the evidence boundary, derives stable IDs, and keeps a full
// reference catalog alongside grouped module edges so every group can expand.
func Build(input Input) (Overview, error) {
	if err := validateScope(input.Scope); err != nil {
		return Overview{}, err
	}
	workspaceID, err := model.HashValue("workspace-v1", map[string]any{
		"worktree_id": input.Scope.WorktreeID, "profile_id": input.Scope.ProfileID,
	})
	if err != nil {
		return Overview{}, fmt.Errorf("%w: workspace ID: %v", ErrInvalidInput, err)
	}
	files, languages, testRoots, capturedBytes, err := indexFiles(input.Files)
	if err != nil {
		return Overview{}, err
	}
	modules, moduleByKey, err := buildModules(input.Scope, files, input.Modules)
	if err != nil {
		return Overview{}, err
	}
	packages, packageByKey, err := buildPackages(files, moduleByKey, input.Packages)
	if err != nil {
		return Overview{}, err
	}
	entryPoints, err := buildEntryPoints(files, packageByKey, input.EntryPoints)
	if err != nil {
		return Overview{}, err
	}
	documents, err := buildDocuments(files, input.TrustedDocuments)
	if err != nil {
		return Overview{}, err
	}
	refs, dependencies, expansions, referenceGaps, err := buildReferences(input.Scope, files, packageByKey, input.References)
	if err != nil {
		return Overview{}, err
	}
	gaps := append([]Gap(nil), input.KnownGaps...)
	gaps = append(gaps, referenceGaps...)
	for _, pkg := range packages {
		if len(pkg.ModuleIDs) == 0 {
			gaps = append(gaps, Gap{Code: "packages_without_declared_module", Count: 1})
		}
	}
	gaps, err = normalizeGaps(gaps)
	if err != nil {
		return Overview{}, err
	}
	attachModuleChildrenAndPackages(&modules, packages)
	packageCounts := countPackageReferences(refs)
	for i := range packages {
		packages[i].IncomingReferenceCount = packageCounts[packages[i].Key].incoming
		packages[i].OutgoingReferenceCount = packageCounts[packages[i].Key].outgoing
	}
	groupedReferenceCount := 0
	for _, dependency := range dependencies {
		groupedReferenceCount += dependency.ReferenceCount
	}
	statistics := Statistics{
		FileCount: len(files), PackageCount: len(packages), ModuleCount: len(modules),
		ReferenceCount: len(refs), GroupedReferenceCount: groupedReferenceCount,
		CatalogOnlyReferenceCount: len(refs) - groupedReferenceCount,
		DependencyGroupCount:      len(dependencies), CapturedBytes: capturedBytes,
	}
	return Overview{
		WorkspaceID: workspaceID, Scope: input.Scope, Languages: languages, Modules: modules, Packages: packages,
		Dependencies: dependencies, ReferenceCatalog: refs, EntryPoints: entryPoints,
		TrustedDocuments: documents, TestRoots: testRoots, Gaps: gaps,
		Capabilities: Capabilities{
			D1Structure: "partial", D2InteractionsAndData: "partial",
			D3BehaviorAndConstraints: "not_available", D4EvolutionAndEvidence: "partial",
		},
		Statistics: statistics, expansions: expansions,
	}, nil
}

// Expand returns all source references represented by a grouped dependency.
// The returned slice is detached from the immutable overview result.
func (overview Overview) Expand(ref model.Hash) ([]Reference, error) {
	items, ok := overview.expansions[ref]
	if !ok {
		return nil, ErrUnknownExpandRef
	}
	result := make([]Reference, len(items))
	for i, item := range items {
		result[i] = cloneReference(item)
	}
	return result, nil
}

func validateScope(scope Scope) error {
	if _, err := model.ParseUUID(string(scope.WorktreeID)); err != nil {
		return fmt.Errorf("%w: worktree: %v", ErrInvalidInput, err)
	}
	if _, err := model.ParseHash(string(scope.ProfileID)); err != nil {
		return fmt.Errorf("%w: profile: %v", ErrInvalidInput, err)
	}
	if _, err := model.ParseUUID(string(scope.GenerationID)); err != nil {
		return fmt.Errorf("%w: generation: %v", ErrInvalidInput, err)
	}
	return nil
}

type fileRecord struct {
	SourceFile
}

func indexFiles(input []SourceFile) (map[string]fileRecord, []string, []string, int64, error) {
	files := make(map[string]fileRecord, len(input))
	languageSet, testRootSet := make(map[string]struct{}), make(map[string]struct{})
	var total int64
	for _, file := range input {
		if err := model.ValidateRelativePath(file.Path); err != nil {
			return nil, nil, nil, 0, fmt.Errorf("%w: file path: %v", ErrInvalidInput, err)
		}
		if _, err := model.ParseHash(string(file.ContentHash)); err != nil {
			return nil, nil, nil, 0, fmt.Errorf("%w: file hash: %v", ErrInvalidInput, err)
		}
		if file.ByteLength < 0 || file.ByteLength > model.MaxSafeInteger || !oneOf(file.Role, "source", "test", "document", "manifest", "config", "other") || (file.Language != "" && !validText(file.Language)) {
			return nil, nil, nil, 0, fmt.Errorf("%w: invalid source file metadata", ErrInvalidInput)
		}
		if _, exists := files[file.Path]; exists {
			return nil, nil, nil, 0, fmt.Errorf("%w: duplicate source path %q", ErrInvalidInput, file.Path)
		}
		if total > model.MaxSafeInteger-file.ByteLength {
			return nil, nil, nil, 0, fmt.Errorf("%w: total source bytes exceed safe integer", ErrInvalidInput)
		}
		files[file.Path] = fileRecord{file}
		total += file.ByteLength
		if file.Language != "" {
			languageSet[file.Language] = struct{}{}
		}
		if file.Role == "test" {
			testRootSet[path.Dir(file.Path)] = struct{}{}
		}
	}
	return files, sortedSet(languageSet), sortedSet(testRootSet), total, nil
}

func buildModules(scope Scope, files map[string]fileRecord, input []ModuleInput) ([]Module, map[string]Module, error) {
	ordered := append([]ModuleInput(nil), input...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	modules := make([]Module, 0, len(ordered))
	byKey := make(map[string]Module, len(ordered))
	for _, item := range ordered {
		if !validText(item.Key) || !validText(item.Label) || !validText(item.Kind) || (item.ParentKey != "" && !validText(item.ParentKey)) || (item.Plane != "intended" && item.Plane != "observed_static") {
			return nil, nil, fmt.Errorf("%w: invalid module identity or plane", ErrInvalidInput)
		}
		if _, exists := byKey[item.Key]; exists {
			return nil, nil, fmt.Errorf("%w: duplicate module key %q", ErrInvalidInput, item.Key)
		}
		if err := validateEvidence(files, item.Source); err != nil {
			return nil, nil, fmt.Errorf("%w: module %q source: %v", ErrInvalidInput, item.Key, err)
		}
		id, err := model.HashValue("module-v1", map[string]any{
			"worktree_id": scope.WorktreeID, "profile_id": scope.ProfileID,
			"module_key": item.Key, "kind": item.Kind, "plane": item.Plane,
			"source_path": item.Source.Path,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("%w: module ID: %v", ErrInvalidInput, err)
		}
		module := Module{ID: id, Key: item.Key, Label: item.Label, Kind: item.Kind, Plane: item.Plane, Source: item.Source,
			PackageKeys: make([]model.Hash, 0), ChildIDs: make([]model.Hash, 0)}
		modules = append(modules, module)
		byKey[item.Key] = module
	}
	for i, item := range ordered {
		if item.ParentKey == "" {
			continue
		}
		parent, ok := byKey[item.ParentKey]
		if !ok || item.ParentKey == item.Key {
			return nil, nil, fmt.Errorf("%w: invalid parent for module %q", ErrInvalidInput, item.Key)
		}
		modules[i].ParentID = hashPointer(parent.ID)
		byKey[item.Key] = modules[i]
	}
	if err := validateModuleTree(ordered); err != nil {
		return nil, nil, err
	}
	return modules, byKey, nil
}

func validateModuleTree(input []ModuleInput) error {
	parents := make(map[string]string, len(input))
	for _, item := range input {
		parents[item.Key] = item.ParentKey
	}
	state := make(map[string]uint8, len(input))
	for _, item := range input {
		chain := make([]string, 0)
		for key := item.Key; key != "" && state[key] == 0; key = parents[key] {
			state[key] = 1
			chain = append(chain, key)
		}
		key := ""
		if len(chain) > 0 {
			key = parents[chain[len(chain)-1]]
		}
		if key != "" && state[key] == 1 {
			return fmt.Errorf("%w: module parent cycle at %q", ErrInvalidInput, key)
		}
		for _, key := range chain {
			state[key] = 2
		}
	}
	return nil
}

func buildPackages(files map[string]fileRecord, modules map[string]Module, input []PackageInput) ([]Package, map[model.Hash]Package, error) {
	ordered := append([]PackageInput(nil), input...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	packages := make([]Package, 0, len(ordered))
	byKey := make(map[model.Hash]Package, len(ordered))
	for _, item := range ordered {
		if _, err := model.ParseHash(string(item.Key)); err != nil || !validText(item.ImportPath) || !validText(item.Variant) || item.SymbolCount < 0 || item.DiagnosticCount < 0 {
			return nil, nil, fmt.Errorf("%w: invalid package identity", ErrInvalidInput)
		}
		if _, exists := byKey[item.Key]; exists {
			return nil, nil, fmt.Errorf("%w: duplicate package key %s", ErrInvalidInput, item.Key)
		}
		memberships := append([]ModuleMembershipInput(nil), item.ModuleMemberships...)
		sort.Slice(memberships, func(i, j int) bool { return memberships[i].ModuleKey < memberships[j].ModuleKey })
		moduleIDs := make([]model.Hash, 0, len(memberships))
		moduleMemberships := make([]ModuleMembership, 0, len(memberships))
		for i, membership := range memberships {
			if !validText(membership.ModuleKey) {
				return nil, nil, fmt.Errorf("%w: invalid package module membership key", ErrInvalidInput)
			}
			if i > 0 && memberships[i-1].ModuleKey == membership.ModuleKey {
				return nil, nil, fmt.Errorf("%w: duplicate package module membership %q", ErrInvalidInput, membership.ModuleKey)
			}
			module, ok := modules[membership.ModuleKey]
			if !ok {
				return nil, nil, fmt.Errorf("%w: package references unknown module %q", ErrInvalidInput, membership.ModuleKey)
			}
			if err := validateEvidence(files, membership.Source); err != nil {
				return nil, nil, fmt.Errorf("%w: package module membership source: %v", ErrInvalidInput, err)
			}
			moduleIDs = append(moduleIDs, module.ID)
			moduleMemberships = append(moduleMemberships, ModuleMembership{ModuleID: module.ID, Source: membership.Source})
		}
		paths := uniqueStrings(item.Files)
		for _, filePath := range paths {
			if _, ok := files[filePath]; !ok {
				return nil, nil, fmt.Errorf("%w: package file %q is outside captured inventory", ErrInvalidInput, filePath)
			}
		}
		pkg := Package{Key: item.Key, ImportPath: item.ImportPath, Variant: item.Variant,
			ModuleIDs: moduleIDs, ModuleMemberships: moduleMemberships, Files: paths,
			SymbolCount: item.SymbolCount, DiagnosticCount: item.DiagnosticCount}
		packages = append(packages, pkg)
		byKey[item.Key] = pkg
	}
	return packages, byKey, nil
}

func buildEntryPoints(files map[string]fileRecord, packages map[model.Hash]Package, input []EntryPointInput) ([]EntryPoint, error) {
	result := make([]EntryPoint, 0, len(input))
	seen := make(map[model.Hash]struct{}, len(input))
	for _, item := range input {
		pkg, ok := packages[item.PackageKey]
		if !ok || !validText(item.Kind) || !validText(item.Name) || !contains(pkg.Files, item.Source.Path) {
			return nil, fmt.Errorf("%w: invalid entry point", ErrInvalidInput)
		}
		if err := validateEvidence(files, item.Source); err != nil {
			return nil, fmt.Errorf("%w: entry point source: %v", ErrInvalidInput, err)
		}
		id, err := model.HashValue("overview-entry-point-v1", map[string]any{
			"package_key": item.PackageKey, "kind": item.Kind, "name": item.Name,
			"path": item.Source.Path, "content_hash": item.Source.ContentHash,
			"start_byte": item.Source.Range.StartByte, "end_byte": item.Source.Range.EndByte,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: entry point ID: %v", ErrInvalidInput, err)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: duplicate entry point", ErrInvalidInput)
		}
		seen[id] = struct{}{}
		result = append(result, EntryPoint{ID: id, PackageKey: item.PackageKey, Kind: item.Kind, Name: item.Name, Source: item.Source})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func buildDocuments(files map[string]fileRecord, input []DocumentInput) ([]TrustedDocument, error) {
	result := make([]TrustedDocument, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, item := range input {
		if !validText(item.Title) || !validText(item.TrustRef) {
			return nil, fmt.Errorf("%w: trusted document needs title and trust reference", ErrInvalidInput)
		}
		if err := validateEvidence(files, item.Source); err != nil {
			return nil, fmt.Errorf("%w: trusted document source: %v", ErrInvalidInput, err)
		}
		key := item.TrustRef + "\x00" + item.Source.Path
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%w: duplicate trusted document", ErrInvalidInput)
		}
		seen[key] = struct{}{}
		result = append(result, TrustedDocument{Title: item.Title, TrustRef: item.TrustRef, Source: item.Source})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TrustRef != result[j].TrustRef {
			return result[i].TrustRef < result[j].TrustRef
		}
		return result[i].Source.Path < result[j].Source.Path
	})
	return result, nil
}

func buildReferences(scope Scope, files map[string]fileRecord, packages map[model.Hash]Package, input []ReferenceInput) ([]Reference, []Dependency, map[model.Hash][]Reference, []Gap, error) {
	ordered := append([]ReferenceInput(nil), input...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	refs := make([]Reference, 0, len(ordered))
	byPackage := packages
	seen := make(map[model.Hash]struct{}, len(ordered))
	groups := make(map[dependencyKey][]Reference)
	gapCounts := make(map[string]int)
	for _, item := range ordered {
		if _, err := model.ParseHash(string(item.ID)); err != nil || !validText(item.Kind) || !validText(item.EvidenceKind) || !oneOf(item.ResolutionStatus, "exact_in_supported_scope", "candidate", "unresolved") {
			return nil, nil, nil, nil, fmt.Errorf("%w: invalid reference identity or relation", ErrInvalidInput)
		}
		if _, exists := seen[item.ID]; exists {
			return nil, nil, nil, nil, fmt.Errorf("%w: duplicate reference ID %s", ErrInvalidInput, item.ID)
		}
		seen[item.ID] = struct{}{}
		sourcePackage, ok := byPackage[item.SourcePackageKey]
		if !ok || !contains(sourcePackage.Files, item.Source.Path) {
			return nil, nil, nil, nil, fmt.Errorf("%w: reference source package does not own its evidence path", ErrInvalidInput)
		}
		if err := validateEvidence(files, item.Source); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("%w: reference source: %v", ErrInvalidInput, err)
		}
		ref := Reference{ID: item.ID, SourcePackageKey: item.SourcePackageKey, Kind: item.Kind,
			EvidenceKind: item.EvidenceKind, ResolutionStatus: item.ResolutionStatus, Source: item.Source,
			SourceModuleIDs: append([]model.Hash(nil), sourcePackage.ModuleIDs...)}
		if item.TargetPackageKey != nil {
			targetKey := *item.TargetPackageKey
			targetPackage, exists := byPackage[targetKey]
			if !exists {
				return nil, nil, nil, nil, fmt.Errorf("%w: reference target package is unknown", ErrInvalidInput)
			}
			ref.TargetPackageKey = hashPointer(targetKey)
			ref.TargetModuleIDs = append([]model.Hash(nil), targetPackage.ModuleIDs...)
		} else if item.ResolutionStatus == "exact_in_supported_scope" {
			return nil, nil, nil, nil, fmt.Errorf("%w: exact reference is missing its target package", ErrInvalidInput)
		}
		refs = append(refs, cloneReference(ref))
		if item.ResolutionStatus == "unresolved" {
			gapCounts["unresolved_references_not_grouped"]++
		} else if len(ref.SourceModuleIDs) != 1 || len(ref.TargetModuleIDs) != 1 {
			gapCounts["references_not_grouped_to_unique_modules"]++
		} else {
			from, to := ref.SourceModuleIDs[0], ref.TargetModuleIDs[0]
			if from != to {
				key := dependencyKey{from: string(from), to: string(to), kind: item.Kind, evidence: item.EvidenceKind, status: item.ResolutionStatus}
				groups[key] = append(groups[key], cloneReference(ref))
			} else {
				gapCounts["intra_module_references_not_shown_as_dependencies"]++
			}
		}
	}
	dependencies := make([]Dependency, 0, len(groups))
	expansions := make(map[model.Hash][]Reference, len(groups))
	keys := make([]dependencyKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return dependencyKeyLess(keys[i], keys[j]) })
	for _, key := range keys {
		references := groups[key]
		sort.Slice(references, func(i, j int) bool { return references[i].ID < references[j].ID })
		id, err := model.HashValue("overview-dependency-v1", map[string]any{
			"worktree_id": scope.WorktreeID, "profile_id": scope.ProfileID, "generation_id": scope.GenerationID,
			"from_module_id": model.Hash(key.from), "to_module_id": model.Hash(key.to),
			"kind": key.kind, "evidence_kind": key.evidence, "resolution_status": key.status,
		})
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("%w: dependency ID: %v", ErrInvalidInput, err)
		}
		expandRef, err := model.HashValue("overview-expand-v1", map[string]any{
			"worktree_id": scope.WorktreeID, "profile_id": scope.ProfileID,
			"generation_id": scope.GenerationID, "dependency_id": id,
		})
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("%w: expansion ID: %v", ErrInvalidInput, err)
		}
		dependencies = append(dependencies, Dependency{ID: id, FromModuleID: model.Hash(key.from), ToModuleID: model.Hash(key.to),
			Kind: key.kind, EvidenceKind: key.evidence, ResolutionStatus: key.status,
			ReferenceCount: len(references), ExpandRef: expandRef})
		expansions[expandRef] = append([]Reference(nil), references...)
	}
	gaps := make([]Gap, 0, len(gapCounts))
	for code, count := range gapCounts {
		gaps = append(gaps, Gap{Code: code, Count: count})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Code < gaps[j].Code })
	return refs, dependencies, expansions, gaps, nil
}

func attachModuleChildrenAndPackages(modules *[]Module, packages []Package) {
	index := make(map[model.Hash]int, len(*modules))
	for i := range *modules {
		index[(*modules)[i].ID] = i
	}
	fileSets := make(map[model.Hash]map[string]struct{}, len(*modules))
	for i := range *modules {
		module := &(*modules)[i]
		if module.ParentID != nil {
			if parentIndex, ok := index[*module.ParentID]; ok {
				(*modules)[parentIndex].ChildIDs = append((*modules)[parentIndex].ChildIDs, module.ID)
			}
		}
	}
	for _, pkg := range packages {
		for _, moduleID := range pkg.ModuleIDs {
			position := index[moduleID]
			(*modules)[position].PackageKeys = append((*modules)[position].PackageKeys, pkg.Key)
			if fileSets[moduleID] == nil {
				fileSets[moduleID] = make(map[string]struct{})
			}
			for _, file := range pkg.Files {
				fileSets[moduleID][file] = struct{}{}
			}
		}
	}
	for i := range *modules {
		module := &(*modules)[i]
		sort.Slice(module.PackageKeys, func(i, j int) bool { return module.PackageKeys[i] < module.PackageKeys[j] })
		sort.Slice(module.ChildIDs, func(i, j int) bool { return module.ChildIDs[i] < module.ChildIDs[j] })
		module.FileCount = len(fileSets[module.ID])
	}
}

type packageReferenceCounts struct {
	incoming int
	outgoing int
}

func countPackageReferences(references []Reference) map[model.Hash]packageReferenceCounts {
	counts := make(map[model.Hash]packageReferenceCounts)
	for _, reference := range references {
		outgoing := counts[reference.SourcePackageKey]
		outgoing.outgoing++
		counts[reference.SourcePackageKey] = outgoing
		if reference.TargetPackageKey != nil {
			incoming := counts[*reference.TargetPackageKey]
			incoming.incoming++
			counts[*reference.TargetPackageKey] = incoming
		}
	}
	return counts
}

func validateEvidence(files map[string]fileRecord, evidence SourceEvidence) error {
	file, ok := files[evidence.Path]
	if !ok || file.ContentHash != evidence.ContentHash || evidence.Range.StartByte < 0 || evidence.Range.EndByte < evidence.Range.StartByte || evidence.Range.EndByte > file.ByteLength {
		return fmt.Errorf("source anchor is not bound to a captured file/hash/range")
	}
	return nil
}

func uniqueStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func contains(values []string, value string) bool {
	i := sort.SearchStrings(values, value)
	return i < len(values) && values[i] == value
}

func hashPointer(value model.Hash) *model.Hash { copy := value; return &copy }

func cloneReference(value Reference) Reference {
	value.TargetPackageKey = cloneHashPointer(value.TargetPackageKey)
	value.SourceModuleIDs = append([]model.Hash(nil), value.SourceModuleIDs...)
	value.TargetModuleIDs = append([]model.Hash(nil), value.TargetModuleIDs...)
	return value
}

func cloneHashPointer(value *model.Hash) *model.Hash {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func dependencyKeyLess(a, b dependencyKey) bool {
	for _, pair := range [][2]string{{a.from, b.from}, {a.to, b.to}, {a.kind, b.kind}, {a.evidence, b.evidence}, {a.status, b.status}} {
		if pair[0] != pair[1] {
			return pair[0] < pair[1]
		}
	}
	return false
}

func normalizeGaps(input []Gap) ([]Gap, error) {
	counts := make(map[string]int, len(input))
	for _, gap := range input {
		if !validText(gap.Code) || gap.Count < 1 {
			return nil, fmt.Errorf("%w: gap needs a code and positive count", ErrInvalidInput)
		}
		current, added := int64(counts[gap.Code]), int64(gap.Count)
		if added > model.MaxSafeInteger || current > model.MaxSafeInteger-added {
			return nil, fmt.Errorf("%w: gap count exceeds safe integer", ErrInvalidInput)
		}
		counts[gap.Code] = int(current + added)
	}
	result := make([]Gap, 0, len(counts))
	for code, count := range counts {
		result = append(result, Gap{Code: code, Count: count})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Code < result[j].Code })
	return result, nil
}

func validText(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value)
}
