package golang

import (
	"fmt"
	"sort"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type CoverageStatus string

const (
	CoverageSupportedScopeComplete CoverageStatus = "supported_scope_complete"
	CoveragePartial                CoverageStatus = "partial"
	CoverageUnsupported            CoverageStatus = "unsupported"
	CoverageUnknown                CoverageStatus = "unknown"
)

// CoverageGap records one known limitation. A nil Count means its size was
// not present in the selected inputs and must not be interpreted as zero.
type CoverageGap struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Count  *int64 `json:"count"`
}

// AnalysisCoverage describes only the explicit package inputs. It never
// implies repository-wide, runtime, assertion, or branch coverage.
type AnalysisCoverage struct {
	Status                    CoverageStatus `json:"status"`
	ScopeDescription          string         `json:"scope_description"`
	PackageCount              int64          `json:"package_count"`
	ProductionPackageCount    int64          `json:"production_package_count"`
	InternalTestPackageCount  int64          `json:"internal_test_package_count"`
	ExternalTestPackageCount  int64          `json:"external_test_package_count"`
	FileCount                 int64          `json:"file_count"`
	ParsedFileCount           int64          `json:"parsed_file_count"`
	PartialFileCount          int64          `json:"partial_file_count"`
	SkippedFileCount          int64          `json:"skipped_file_count"`
	TypedCompletePackageCount int64          `json:"typed_complete_package_count"`
	TypedPartialPackageCount  int64          `json:"typed_partial_package_count"`
	TypedNotRequestedCount    int64          `json:"typed_not_requested_count"`
	RelationOccurrenceCount   int64          `json:"relation_occurrence_count"`
	TestReferenceCount        int64          `json:"test_reference_count"`
	UnresolvedCallCount       int64          `json:"unresolved_call_count"`
	ExternalTargetCount       int64          `json:"external_target_count"`
	DiagnosticCount           int64          `json:"diagnostic_count"`
	Gaps                      []CoverageGap  `json:"gaps"`
}

func coverageForAnalysis(packages []PackageFacts, relations []RelationOccurrence, runStatus LoadStatus, loadDiagnostics []LoadDiagnostic) AnalysisCoverage {
	coverage := AnalysisCoverage{
		Status:                  CoverageUnknown,
		ScopeDescription:        "Go static analysis of the captured files in the explicitly supplied package variants and selected profile; not a repository inventory, execution, assertion, or branch-coverage result.",
		PackageCount:            int64(len(packages)),
		RelationOccurrenceCount: int64(len(relations)),
		Gaps:                    make([]CoverageGap, 0),
	}
	addGap := func(code, reason string, count *int64) {
		for index := range coverage.Gaps {
			if coverage.Gaps[index].Code != code {
				continue
			}
			if coverage.Gaps[index].Count != nil && count != nil {
				combined := *coverage.Gaps[index].Count + *count
				coverage.Gaps[index].Count = &combined
			} else {
				coverage.Gaps[index].Count = nil
			}
			return
		}
		coverage.Gaps = append(coverage.Gaps, CoverageGap{Code: code, Reason: reason, Count: cloneCount(count)})
	}
	addGap("captured_package_scope_only", "The denominator is limited to the PackageInput files and variants supplied to this analysis; repository inventory was not supplied.", nil)
	addGap("inventory_boundaries_not_attached", "Generated candidates, vendor exclusions, sparse omissions, submodules, and nested repositories are not enumerated by this captured-source analyzer.", nil)
	addGap("non_go_semantics_not_analyzed", "TypeScript, SQL, configuration text, and external-language contracts are outside this Go analyzer; lexical matches are not semantic relations.", nil)
	addGap("runtime_test_coverage_not_collected", "A TEST_REF records a static source reference only; no test execution, assertion, branch coverage, or runtime behavior was observed.", nil)
	addGap("dynamic_runtime_targets_not_proven", "Reflection, function values, plugins, and runtime dispatch remain unknown unless represented by an explicit unresolved boundary.", nil)

	if len(packages) == 0 {
		addGap("empty_analysis_scope", "No package facts were supplied, so coverage is unknown rather than complete or dependency-free.", nil)
		return coverage
	}

	complete := runStatus == LoadComplete && len(loadDiagnostics) == 0
	var packageDiagnosticCount int64
	for _, pkg := range packages {
		switch pkg.Variant {
		case VariantProduction:
			coverage.ProductionPackageCount++
		case VariantInternalTest:
			coverage.InternalTestPackageCount++
		case VariantExternalTest:
			coverage.ExternalTestPackageCount++
		}
		coverage.FileCount += int64(len(pkg.Files))
		coverage.DiagnosticCount += int64(len(pkg.Diagnostics))
		packageDiagnosticCount += int64(len(pkg.Diagnostics))
		if pkg.SyntaxStatus != SyntaxComplete || len(pkg.Diagnostics) > 0 {
			complete = false
		}
		for _, file := range pkg.Files {
			if file.ParseStatus == ParseComplete {
				coverage.ParsedFileCount++
			} else {
				coverage.PartialFileCount++
				if file.ParseStatus == ParseSkipped {
					coverage.SkippedFileCount++
				}
			}
		}
		switch pkg.TypedStatus {
		case TypedComplete:
			coverage.TypedCompletePackageCount++
		case TypedPartial:
			coverage.TypedPartialPackageCount++
			complete = false
		case TypedNotRequested:
			coverage.TypedNotRequestedCount++
			complete = false
		}
	}
	coverage.DiagnosticCount += int64(len(loadDiagnostics))
	for _, relation := range relations {
		switch relation.Kind {
		case RelationTestRef:
			coverage.TestReferenceCount++
		case RelationUnresolvedCall:
			coverage.UnresolvedCallCount++
		}
		if relation.TargetSymbolID != nil && relation.TargetOccurrenceID == nil {
			coverage.ExternalTargetCount++
		}
	}

	if coverage.PartialFileCount > 0 {
		addGap("files_not_fully_parsed", "At least one captured Go file was partial, failed to parse, or exceeded its configured size limit.", countPointer(coverage.PartialFileCount))
	}
	if packageDiagnosticCount > 0 {
		addGap("package_diagnostics_present", "The supplied package facts contain parser or analyzer diagnostics; inspect their paths and codes before relying on completeness.", countPointer(packageDiagnosticCount))
	}
	if coverage.TypedPartialPackageCount > 0 {
		addGap("typed_facts_partial", "One or more package variants lack complete Go type facts; syntax facts and diagnostics remain available.", countPointer(coverage.TypedPartialPackageCount))
	}
	if coverage.TypedNotRequestedCount > 0 {
		addGap("typed_facts_not_requested", "Type analysis was not requested for one or more package variants.", countPointer(coverage.TypedNotRequestedCount))
	}
	if coverage.UnresolvedCallCount > 0 {
		addGap("unresolved_call_targets", "Unresolved call relations are retained as boundaries and are not treated as zero impact.", countPointer(coverage.UnresolvedCallCount))
	}
	if coverage.ExternalTargetCount > 0 {
		addGap("target_source_not_in_selected_packages", "Some statically referenced target symbols have no declaration occurrence in the supplied package inputs.", countPointer(coverage.ExternalTargetCount))
	}
	if len(loadDiagnostics) > 0 || runStatus == LoadPartial {
		count := int64(len(loadDiagnostics))
		var countRef *int64
		if len(loadDiagnostics) > 0 {
			countRef = &count
		}
		addGap("package_load_partial", "The Go package loader reported an incomplete or unavailable package/dependency closure; inspect retained load diagnostics.", countRef)
	}
	if len(relations) == 0 {
		addGap("empty_relation_result_is_scoped", "No relation occurrences were emitted for this explicit scope; this does not prove the repository or runtime has no dependencies.", nil)
	}
	if complete {
		coverage.Status = CoverageSupportedScopeComplete
	} else {
		coverage.Status = CoveragePartial
	}
	sort.Slice(coverage.Gaps, func(i, j int) bool { return coverage.Gaps[i].Code < coverage.Gaps[j].Code })
	return coverage
}

func appendTestReferences(packages []PackageFacts, inputs map[model.Hash]PackageInput) error {
	symbolOccurrences := make(map[model.Hash]model.Hash)
	productionByOccurrence := make(map[model.Hash]model.Hash)
	for _, pkg := range packages {
		for _, declaration := range pkg.Declarations {
			if previous, exists := symbolOccurrences[declaration.SymbolID]; exists && previous != declaration.OccurrenceID {
				return fmt.Errorf("%w: one symbol identity has multiple source occurrences", ErrInvalidInput)
			}
			symbolOccurrences[declaration.SymbolID] = declaration.OccurrenceID
			if pkg.Variant != VariantProduction {
				continue
			}
			if previous, exists := productionByOccurrence[declaration.OccurrenceID]; exists && previous != declaration.SymbolID {
				return fmt.Errorf("%w: one production occurrence has multiple symbol identities", ErrInvalidInput)
			}
			productionByOccurrence[declaration.OccurrenceID] = declaration.SymbolID
		}
	}

	for index := range packages {
		facts := &packages[index]
		if facts.Variant == VariantProduction {
			continue
		}
		input, ok := inputs[facts.Key]
		if !ok {
			return fmt.Errorf("%w: test variant has no matching captured input", ErrInvalidInput)
		}
		captured := capturedFiles(input)
		seen := make(map[model.Hash]struct{})
		for _, existing := range facts.Relations {
			seen[existing.RelationID] = struct{}{}
		}
		for _, use := range facts.Uses {
			if !strings.HasSuffix(use.Path, "_test.go") {
				continue
			}
			occurrence, exists := symbolOccurrences[use.SymbolID]
			if !exists {
				continue
			}
			target, exists := productionByOccurrence[occurrence]
			if !exists {
				continue
			}
			file, exists := captured[use.Path]
			if !exists {
				return fmt.Errorf("%w: test reference path %s is not in captured input", ErrCapturedSource, use.Path)
			}
			if err := use.Range.ValidateAgainst(file.Source); err != nil {
				return fmt.Errorf("%w: test reference range does not bind %s", ErrCapturedSource, use.Path)
			}
			source := testReferenceOwner(*facts, use)
			relation, err := NewRelationFact(RelationInput{
				SourceSymbolID: source, TargetSymbolID: &target,
				Kind: RelationTestRef, EvidenceKind: EvidenceTypeResolved,
				SitePath: use.Path, Range: use.Range,
				Reason: "go/types resolved an identifier in captured test source to a production declaration; this is not execution or coverage evidence",
			}, file.Source)
			if err != nil {
				return fmt.Errorf("create Go test reference: %w", err)
			}
			if _, duplicate := seen[relation.RelationID]; duplicate {
				continue
			}
			seen[relation.RelationID] = struct{}{}
			facts.Relations = append(facts.Relations, relation)
		}
		sortRelationFacts(facts.Relations)
	}
	return nil
}

func testReferenceOwner(facts PackageFacts, use UseFact) model.Hash {
	owner := facts.Key
	ownerRange := int64(^uint64(0) >> 1)
	for _, declaration := range facts.Declarations {
		if declaration.Path != use.Path || (declaration.Kind != "function" && declaration.Kind != "method" && declaration.Kind != "closure") {
			continue
		}
		range_ := declaration.DeclarationRange
		if range_.StartByte > use.Range.StartByte || range_.EndByte < use.Range.EndByte {
			continue
		}
		width := range_.EndByte - range_.StartByte
		if width < ownerRange {
			owner, ownerRange = declaration.SymbolID, width
		}
	}
	return owner
}

func countPointer(value int64) *int64 {
	copy := value
	return &copy
}

func cloneCount(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return countPointer(*value)
}
