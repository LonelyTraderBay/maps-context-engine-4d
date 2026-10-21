package history

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrSnapshotUnavailable   = errors.New("semantic snapshot unavailable")
	ErrInvalidSnapshotDiff   = errors.New("invalid semantic snapshot diff")
	ErrSnapshotScopeMismatch = errors.New("semantic snapshots have incompatible worktree scope")
)

type SnapshotAvailability string

const (
	SnapshotRetained    SnapshotAvailability = "retained"
	SnapshotMissing     SnapshotAvailability = "missing"
	SnapshotExpired     SnapshotAvailability = "expired"
	SnapshotDenied      SnapshotAvailability = "access_denied"
	SnapshotUnavailable SnapshotAvailability = "unavailable"
)

type DiffScope string

const (
	DiffScopeComplete DiffScope = "complete"
	DiffScopePartial  DiffScope = "partial"
	DiffScopeUnknown  DiffScope = "unknown"
)

// SemanticSnapshot is an immutable, in-memory view of retained snapshot data.
// It contains no Git reader or latest-snapshot lookup; callers must resolve the
// requested retained snapshots before calling CompareSemanticSnapshots.
type SemanticSnapshot struct {
	Availability   SnapshotAvailability
	Scope          DiffScope
	WorktreeID     model.UUID
	ProfileID      model.Hash
	GenerationID   model.UUID
	SnapshotDigest model.Hash
	PolicyDigest   model.Hash
	ProviderDigest model.Hash
	SourceEvidence []model.SourceRef
	Symbols        []DiffSymbol
	Relations      []DiffRelation
	Contracts      []SemanticFact
	Constraints    []Version
	Consumers      []DiffConsumer
}

// DiffSymbol carries multiple independent identity signals. ID is local to a
// generation and is never used as cross-generation identity proof.
type DiffSymbol struct {
	ID            string
	Kind          string
	Name          string
	QualifiedName string
	Owner         string
	Signature     string
	SemanticHash  model.Hash
	Source        model.SourceRef
}

type SymbolMatchStatus string

const (
	SymbolExact     SymbolMatchStatus = "exact"
	SymbolProbable  SymbolMatchStatus = "probable"
	SymbolAmbiguous SymbolMatchStatus = "ambiguous"
	SymbolUnmatched SymbolMatchStatus = "unmatched"
)

type SymbolMapping struct {
	LeftID              string
	RightID             string
	Status              SymbolMatchStatus
	Evidence            []string
	Candidates          []string
	CandidatesTruncated bool
}

type SymbolChange struct {
	Before          DiffSymbol
	After           DiffSymbol
	ChangedFields   []string
	LocationChanged bool
}

// DiffRelation uses generation-scoped endpoint IDs. The diff only compares a
// relation when both endpoints have exact symbol mappings.
type DiffRelation struct {
	ID             string
	SourceSymbolID string
	TargetSymbolID string
	Kind           string
	SemanticHash   model.Hash
}

// SemanticFact is a contract or other stable-key fact. Key is supplied by the
// producer from its declared contract identity; it is not inferred from name.
type SemanticFact struct {
	ID           string
	Key          string
	SemanticHash model.Hash
}

type DiffFact struct {
	ID           string
	Key          string
	SemanticHash model.Hash
}

type FactChange struct {
	Before DiffFact
	After  DiffFact
}

type FactDelta struct {
	Added     []DiffFact
	Removed   []DiffFact
	Changed   []FactChange
	Uncertain []DiffFact
}

type DiffConsumer struct {
	ID             string
	Name           string
	DependencyKeys []string
}

type AffectedConsumer struct {
	ID             string
	Name           string
	DependencyKeys []string
}

type ProviderProfileDelta struct {
	ProfileChanged  bool
	ProviderChanged bool
	PolicyChanged   bool
}

type DecisionEvaluation struct {
	RecordID         model.UUID
	Version          int64
	DeclaredStatus   Status
	EffectiveStatus  Status
	ProvenanceStatus ProvenanceStatus
}

type DecisionChange struct {
	Before DecisionEvaluation
	After  DecisionEvaluation
}

type ConstraintDelta struct {
	Added   []DecisionEvaluation
	Removed []DecisionEvaluation
	Changed []DecisionChange
}

type SemanticDiff struct {
	Compatibility      ProviderProfileDelta
	Comparable         bool
	SymbolMappings     []SymbolMapping
	AddedSymbols       []DiffSymbol
	RemovedSymbols     []DiffSymbol
	ChangedSymbols     []SymbolChange
	UncertainSymbols   []DiffSymbol
	Relations          FactDelta
	UncertainRelations []DiffRelation
	Contracts          FactDelta
	Constraints        ConstraintDelta
	AffectedConsumers  []AffectedConsumer
}

type SnapshotUnavailableError struct {
	Side   string
	Status SnapshotAvailability
}

func (err *SnapshotUnavailableError) Error() string {
	return fmt.Sprintf("%s semantic snapshot is %q: %v", err.Side, err.Status, ErrSnapshotUnavailable)
}

func (err *SnapshotUnavailableError) Unwrap() error { return ErrSnapshotUnavailable }

// CompareSemanticSnapshots compares only the two supplied retained snapshots.
// A profile or provider change is returned as a separate delta and suppresses
// graph additions/removals because those outputs are not comparable.
func CompareSemanticSnapshots(left, right *SemanticSnapshot) (SemanticDiff, error) {
	if err := requireRetainedSnapshot("left", left); err != nil {
		return SemanticDiff{}, err
	}
	if err := requireRetainedSnapshot("right", right); err != nil {
		return SemanticDiff{}, err
	}
	if err := left.validate(); err != nil {
		return SemanticDiff{}, err
	}
	if err := right.validate(); err != nil {
		return SemanticDiff{}, err
	}
	if left.WorktreeID != right.WorktreeID {
		return SemanticDiff{}, fmt.Errorf("left and right worktrees differ: %w", ErrSnapshotScopeMismatch)
	}

	result := SemanticDiff{Compatibility: ProviderProfileDelta{
		ProfileChanged:  left.ProfileID != right.ProfileID,
		ProviderChanged: left.ProviderDigest != right.ProviderDigest,
		PolicyChanged:   left.PolicyDigest != right.PolicyDigest,
	}}
	if result.Compatibility.ProfileChanged || result.Compatibility.ProviderChanged {
		if err := compareConstraints(left, right, &result); err != nil {
			return SemanticDiff{}, err
		}
		result.AffectedConsumers = findAffectedConsumers(left, right, &result)
		return result, nil
	}
	result.Comparable = true

	leftScope, rightScope := left.Scope, right.Scope
	if result.Compatibility.PolicyChanged {
		// A capture-policy change can alter which facts were visible. Keep known
		// matches, but do not infer additions or removals across that boundary.
		leftScope, rightScope = DiffScopeUnknown, DiffScopeUnknown
	}
	leftToRight, rightToLeft := matchSymbols(left.Symbols, right.Symbols, leftScope, rightScope, &result)
	if err := compareRelations(left, right, leftToRight, rightToLeft, leftScope, rightScope, &result); err != nil {
		return SemanticDiff{}, err
	}
	result.Contracts = compareFacts(left.Contracts, right.Contracts, leftScope, rightScope)
	if err := compareConstraints(left, right, &result); err != nil {
		return SemanticDiff{}, err
	}
	result.AffectedConsumers = findAffectedConsumers(left, right, &result)
	return result, nil
}

func requireRetainedSnapshot(side string, snapshot *SemanticSnapshot) error {
	if snapshot == nil {
		return &SnapshotUnavailableError{Side: side, Status: SnapshotMissing}
	}
	if snapshot.Availability != SnapshotRetained {
		status := snapshot.Availability
		if status == "" {
			status = SnapshotUnavailable
		}
		return &SnapshotUnavailableError{Side: side, Status: status}
	}
	return nil
}

func (snapshot *SemanticSnapshot) validate() error {
	invalid := func(reason string) error { return fmt.Errorf("%s: %w", reason, ErrInvalidSnapshotDiff) }
	if snapshot.Scope != DiffScopeComplete && snapshot.Scope != DiffScopePartial && snapshot.Scope != DiffScopeUnknown {
		return invalid("snapshot scope is invalid")
	}
	if _, err := model.ParseUUID(string(snapshot.WorktreeID)); err != nil {
		return invalid("worktree identity is invalid")
	}
	if _, err := model.ParseUUID(string(snapshot.GenerationID)); err != nil {
		return invalid("generation identity is invalid")
	}
	for _, digest := range []model.Hash{snapshot.ProfileID, snapshot.SnapshotDigest, snapshot.PolicyDigest, snapshot.ProviderDigest} {
		if _, err := model.ParseHash(string(digest)); err != nil {
			return invalid("snapshot digest or profile identity is invalid")
		}
	}
	for index, ref := range snapshot.SourceEvidence {
		if ValidateSourceRef(ref) != nil || ref.WorktreeID != snapshot.WorktreeID || ref.ProfileID != snapshot.ProfileID || ref.GenerationID != snapshot.GenerationID {
			return invalid(fmt.Sprintf("source evidence %d is not bound to the snapshot", index))
		}
	}
	seenSymbols := make(map[string]struct{}, len(snapshot.Symbols))
	for _, symbol := range snapshot.Symbols {
		if symbol.ID == "" || symbol.Kind == "" || symbol.Name == "" || !validDiffText(symbol.ID) || !validDiffText(symbol.Kind) || !validDiffText(symbol.Name) {
			return invalid("symbol identity is incomplete")
		}
		if _, exists := seenSymbols[symbol.ID]; exists {
			return invalid("symbol IDs must be unique within a generation")
		}
		seenSymbols[symbol.ID] = struct{}{}
		if err := validateSnapshotSource(snapshot, symbol.Source); err != nil {
			return invalid("symbol source is not bound to the snapshot")
		}
		if err := validateOptionalHash(symbol.SemanticHash); err != nil {
			return invalid("symbol semantic hash is invalid")
		}
	}
	seenRelations := make(map[string]struct{}, len(snapshot.Relations))
	for _, relation := range snapshot.Relations {
		if relation.ID == "" || relation.Kind == "" || !validDiffText(relation.ID) || !validDiffText(relation.Kind) {
			return invalid("relation identity is incomplete")
		}
		if _, exists := seenRelations[relation.ID]; exists {
			return invalid("relation IDs must be unique within a generation")
		}
		seenRelations[relation.ID] = struct{}{}
		if _, exists := seenSymbols[relation.SourceSymbolID]; !exists {
			return invalid("relation source symbol is unavailable")
		}
		if relation.TargetSymbolID != "" {
			if _, exists := seenSymbols[relation.TargetSymbolID]; !exists {
				return invalid("relation target symbol is unavailable")
			}
		}
		if err := validateOptionalHash(relation.SemanticHash); err != nil {
			return invalid("relation semantic hash is invalid")
		}
	}
	if err := validateFacts(snapshot.Contracts); err != nil {
		return err
	}
	if err := validateConsumers(snapshot.Consumers); err != nil {
		return err
	}
	seenDecisions := make(map[model.UUID]struct{}, len(snapshot.Constraints))
	for _, record := range snapshot.Constraints {
		if _, exists := seenDecisions[record.RecordID]; exists {
			return invalid("only one immutable version per decision is allowed in a snapshot")
		}
		seenDecisions[record.RecordID] = struct{}{}
		if err := validateDecisionRecord(record); err != nil {
			return invalid("constraint record is invalid")
		}
	}
	return nil
}

func validateSnapshotSource(snapshot *SemanticSnapshot, ref model.SourceRef) error {
	if err := ValidateSourceRef(ref); err != nil {
		return err
	}
	if ref.WorktreeID != snapshot.WorktreeID || ref.ProfileID != snapshot.ProfileID || ref.GenerationID != snapshot.GenerationID {
		return ErrInvalidSnapshotDiff
	}
	return nil
}

func validDiffText(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func validateOptionalHash(value model.Hash) error {
	if value == "" {
		return nil
	}
	_, err := model.ParseHash(string(value))
	return err
}

func validateFacts(facts []SemanticFact) error {
	seen := make(map[string]struct{}, len(facts))
	for _, fact := range facts {
		if !validDiffText(fact.ID) || !validDiffText(fact.Key) || fact.SemanticHash == "" {
			return fmt.Errorf("fact identity or hash is incomplete: %w", ErrInvalidSnapshotDiff)
		}
		if _, err := model.ParseHash(string(fact.SemanticHash)); err != nil {
			return fmt.Errorf("fact hash is invalid: %w", ErrInvalidSnapshotDiff)
		}
		if _, exists := seen[fact.Key]; exists {
			return fmt.Errorf("fact keys must be unique within a snapshot: %w", ErrInvalidSnapshotDiff)
		}
		seen[fact.Key] = struct{}{}
	}
	return nil
}

func validateConsumers(consumers []DiffConsumer) error {
	seen := make(map[string]struct{}, len(consumers))
	for _, consumer := range consumers {
		if !validDiffText(consumer.ID) || !validDiffText(consumer.Name) {
			return fmt.Errorf("consumer identity is incomplete: %w", ErrInvalidSnapshotDiff)
		}
		if _, exists := seen[consumer.ID]; exists {
			return fmt.Errorf("consumer IDs must be unique: %w", ErrInvalidSnapshotDiff)
		}
		seen[consumer.ID] = struct{}{}
		keys := make(map[string]struct{}, len(consumer.DependencyKeys))
		for _, key := range consumer.DependencyKeys {
			if !validDiffText(key) {
				return fmt.Errorf("consumer dependency key is invalid: %w", ErrInvalidSnapshotDiff)
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("consumer dependency keys must be unique: %w", ErrInvalidSnapshotDiff)
			}
			keys[key] = struct{}{}
		}
	}
	return nil
}

const maxSymbolCandidatePreview = 32

type symbolIndexKey struct {
	Kind       string
	Signal     string
	First      string
	Second     string
	Third      string
	Fourth     string
	OwnerAware bool
}

type symbolCandidateIndex struct {
	plain      map[symbolIndexKey][]int
	owners     map[symbolIndexKey]map[string][]int
	ownerNames map[symbolIndexKey][]string
}

type symbolCandidate struct {
	ID       string
	Evidence []string
}

type symbolCandidateSet struct {
	Items     []symbolCandidate
	Truncated bool
}

func matchSymbols(left, right []DiffSymbol, leftScope, rightScope DiffScope, result *SemanticDiff) (map[string]string, map[string]string) {
	leftToRight := make(map[string]string)
	rightToLeft := make(map[string]string)
	matchedLeft := make(map[string]bool)
	matchedRight := make(map[string]bool)

	leftExact := collectSymbolCandidates(left, right, buildSymbolCandidateIndex(right, exactSymbolIndexKeys), exactSymbolIndexKeys, exactSymbolEvidence)
	rightExact := collectSymbolCandidates(right, left, buildSymbolCandidateIndex(left, exactSymbolIndexKeys), exactSymbolIndexKeys, exactSymbolEvidence)
	for _, before := range left {
		candidates := leftExact[before.ID]
		if candidates.Truncated || len(candidates.Items) != 1 {
			continue
		}
		afterID := candidates.Items[0].ID
		reverse := rightExact[afterID]
		if reverse.Truncated || len(reverse.Items) != 1 || reverse.Items[0].ID != before.ID {
			continue
		}
		leftToRight[before.ID], rightToLeft[afterID] = afterID, before.ID
		matchedLeft[before.ID], matchedRight[afterID] = true, true
		result.SymbolMappings = append(result.SymbolMappings, SymbolMapping{
			LeftID: before.ID, RightID: afterID, Status: SymbolExact, Evidence: candidates.Items[0].Evidence,
		})
	}

	unmatchedLeft := filterUnmatchedSymbols(left, matchedLeft)
	unmatchedRight := filterUnmatchedSymbols(right, matchedRight)
	leftCandidates := collectSymbolCandidates(unmatchedLeft, unmatchedRight, buildSymbolCandidateIndex(unmatchedRight, probableSymbolIndexKeys), probableSymbolIndexKeys, probableSymbolEvidence)
	rightCandidates := collectSymbolCandidates(unmatchedRight, unmatchedLeft, buildSymbolCandidateIndex(unmatchedLeft, probableSymbolIndexKeys), probableSymbolIndexKeys, probableSymbolEvidence)

	for _, before := range unmatchedLeft {
		candidates := leftCandidates[before.ID]
		switch {
		case len(candidates.Items) == 0:
			result.SymbolMappings = append(result.SymbolMappings, SymbolMapping{LeftID: before.ID, Status: SymbolUnmatched})
			if rightScope == DiffScopeComplete {
				result.RemovedSymbols = append(result.RemovedSymbols, before)
			} else {
				result.UncertainSymbols = append(result.UncertainSymbols, before)
			}
		case !candidates.Truncated && len(candidates.Items) == 1:
			afterID := candidates.Items[0].ID
			reverse := rightCandidates[afterID]
			if !reverse.Truncated && len(reverse.Items) == 1 && reverse.Items[0].ID == before.ID {
				matchedLeft[before.ID], matchedRight[afterID] = true, true
				result.SymbolMappings = append(result.SymbolMappings, SymbolMapping{
					LeftID: before.ID, RightID: afterID, Status: SymbolProbable, Evidence: candidates.Items[0].Evidence,
				})
				continue
			}
			result.SymbolMappings = append(result.SymbolMappings, SymbolMapping{
				LeftID: before.ID, Status: SymbolAmbiguous, Candidates: candidateIDs(candidates), CandidatesTruncated: candidates.Truncated,
			})
			result.UncertainSymbols = append(result.UncertainSymbols, before)
		default:
			result.SymbolMappings = append(result.SymbolMappings, SymbolMapping{
				LeftID: before.ID, Status: SymbolAmbiguous, Candidates: candidateIDs(candidates), CandidatesTruncated: candidates.Truncated,
			})
			result.UncertainSymbols = append(result.UncertainSymbols, before)
		}
	}

	for _, after := range unmatchedRight {
		if matchedRight[after.ID] {
			continue
		}
		candidates := rightCandidates[after.ID]
		if len(candidates.Items) > 0 {
			result.UncertainSymbols = append(result.UncertainSymbols, after)
			continue
		}
		result.SymbolMappings = append(result.SymbolMappings, SymbolMapping{RightID: after.ID, Status: SymbolUnmatched})
		if leftScope == DiffScopeComplete {
			result.AddedSymbols = append(result.AddedSymbols, after)
		} else {
			result.UncertainSymbols = append(result.UncertainSymbols, after)
		}
	}

	leftByID := make(map[string]DiffSymbol, len(left))
	rightByID := make(map[string]DiffSymbol, len(right))
	for _, symbol := range left {
		leftByID[symbol.ID] = symbol
	}
	for _, symbol := range right {
		rightByID[symbol.ID] = symbol
	}
	for leftID, rightID := range leftToRight {
		before, after := leftByID[leftID], rightByID[rightID]
		fields := changedSymbolFields(before, after)
		locationChanged := before.Source.Path != after.Source.Path || before.Source.Range != after.Source.Range
		if len(fields) > 0 || locationChanged {
			result.ChangedSymbols = append(result.ChangedSymbols, SymbolChange{Before: before, After: after, ChangedFields: fields, LocationChanged: locationChanged})
		}
	}
	sortSemanticDiff(result)
	return leftToRight, rightToLeft
}

func buildSymbolCandidateIndex(symbols []DiffSymbol, keyFunc func(DiffSymbol) []symbolIndexKey) symbolCandidateIndex {
	index := symbolCandidateIndex{
		plain:      make(map[symbolIndexKey][]int),
		owners:     make(map[symbolIndexKey]map[string][]int),
		ownerNames: make(map[symbolIndexKey][]string),
	}
	for position, symbol := range symbols {
		for _, key := range keyFunc(symbol) {
			if key.OwnerAware {
				if index.owners[key] == nil {
					index.owners[key] = make(map[string][]int)
				}
				index.owners[key][symbol.Owner] = append(index.owners[key][symbol.Owner], position)
				continue
			}
			index.plain[key] = append(index.plain[key], position)
		}
	}
	for key := range index.plain {
		sort.Slice(index.plain[key], func(i, j int) bool { return symbols[index.plain[key][i]].ID < symbols[index.plain[key][j]].ID })
	}
	for key, owners := range index.owners {
		names := make([]string, 0, len(owners))
		for owner := range owners {
			names = append(names, owner)
			sort.Slice(owners[owner], func(i, j int) bool { return symbols[owners[owner][i]].ID < symbols[owners[owner][j]].ID })
		}
		sort.Strings(names)
		index.owners[key] = owners
		index.ownerNames[key] = names
	}
	return index
}

func collectSymbolCandidates(subjects, opposite []DiffSymbol, index symbolCandidateIndex, keyFunc func(DiffSymbol) []symbolIndexKey, evidenceFunc func(DiffSymbol, DiffSymbol) []string) map[string]symbolCandidateSet {
	sets := make(map[string]symbolCandidateSet, len(subjects))
	for _, subject := range subjects {
		found := make(map[string][]string)
		truncated := false
		for _, key := range keyFunc(subject) {
			complete := index.forEachPosition(key, subject.Owner, func(position int) bool {
				candidate := opposite[position]
				evidence := evidenceFunc(subject, candidate)
				if len(evidence) == 0 {
					return true
				}
				found[candidate.ID] = evidence
				if len(found) > maxSymbolCandidatePreview {
					return false
				}
				return true
			})
			if !complete {
				truncated = true
				break
			}
		}
		ids := make([]string, 0, len(found))
		for id := range found {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > maxSymbolCandidatePreview {
			ids = ids[:maxSymbolCandidatePreview]
			truncated = true
		}
		set := symbolCandidateSet{Truncated: truncated, Items: make([]symbolCandidate, 0, len(ids))}
		for _, id := range ids {
			set.Items = append(set.Items, symbolCandidate{ID: id, Evidence: found[id]})
		}
		sets[subject.ID] = set
	}
	return sets
}

func (index symbolCandidateIndex) forEachPosition(key symbolIndexKey, owner string, visit func(int) bool) bool {
	if !key.OwnerAware {
		for _, position := range index.plain[key] {
			if !visit(position) {
				return false
			}
		}
		return true
	}
	owners := index.owners[key]
	if owner != "" {
		for _, name := range []string{owner, ""} {
			for _, position := range owners[name] {
				if !visit(position) {
					return false
				}
			}
		}
		return true
	}
	for _, name := range index.ownerNames[key] {
		for _, position := range owners[name] {
			if !visit(position) {
				return false
			}
		}
	}
	return true
}

func filterUnmatchedSymbols(symbols []DiffSymbol, matched map[string]bool) []DiffSymbol {
	filtered := make([]DiffSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		if !matched[symbol.ID] {
			filtered = append(filtered, symbol)
		}
	}
	return filtered
}

func candidateIDs(set symbolCandidateSet) []string {
	ids := make([]string, len(set.Items))
	for index, item := range set.Items {
		ids[index] = item.ID
	}
	return ids
}

func exactSymbolIndexKeys(symbol DiffSymbol) []symbolIndexKey {
	var keys []symbolIndexKey
	if symbol.QualifiedName != "" && symbol.Signature != "" {
		keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "qualified_signature", First: symbol.QualifiedName, Second: symbol.Signature, OwnerAware: true})
	}
	if symbol.QualifiedName != "" {
		keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "qualified_source", First: symbol.QualifiedName, Second: string(symbol.Source.ContentHash), Third: rangeKey(symbol.Source.Range), OwnerAware: true})
	}
	if symbol.Owner != "" && symbol.Name != "" && symbol.Signature != "" {
		keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "owner_name_signature_source", First: symbol.Owner, Second: symbol.Name, Third: symbol.Signature, Fourth: string(symbol.Source.ContentHash) + "/" + rangeKey(symbol.Source.Range)})
	}
	return keys
}

func probableSymbolIndexKeys(symbol DiffSymbol) []symbolIndexKey {
	var keys []symbolIndexKey
	if symbol.QualifiedName != "" {
		keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "qualified_name", First: symbol.QualifiedName})
	}
	if symbol.Owner != "" {
		keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "owner_name", First: symbol.Owner, Second: symbol.Name})
	}
	if symbol.Signature != "" {
		keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "signature", First: symbol.Signature})
	}
	keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "source_lineage", First: string(symbol.Source.ContentHash), Second: rangeKey(symbol.Source.Range)})
	keys = append(keys, symbolIndexKey{Kind: symbol.Kind, Signal: "name", First: symbol.Name})
	return keys
}

func rangeKey(value model.ByteRange) string {
	return fmt.Sprintf("%d:%d", value.StartByte, value.EndByte)
}

func exactSymbolEvidence(left, right DiffSymbol) []string {
	if left.Kind != right.Kind {
		return nil
	}
	if left.Owner != "" && right.Owner != "" && left.Owner != right.Owner {
		return nil
	}
	evidence := make([]string, 0, 2)
	if left.QualifiedName != "" && left.QualifiedName == right.QualifiedName {
		evidence = append(evidence, "qualified_declaration")
		if left.Signature != "" && left.Signature == right.Signature {
			evidence = append(evidence, "signature")
		}
		if sourceLineageEqual(left.Source, right.Source) {
			evidence = append(evidence, "source_content_and_range")
		}
	}
	if left.Owner != "" && left.Owner == right.Owner && left.Name == right.Name && left.Signature != "" && left.Signature == right.Signature && sourceLineageEqual(left.Source, right.Source) {
		evidence = append(evidence, "owner_name_signature", "source_content_and_range")
	}
	if len(evidence) < 2 {
		return nil
	}
	return uniqueStrings(evidence)
}

func probableSymbolEvidence(left, right DiffSymbol) []string {
	if left.Kind != right.Kind {
		return nil
	}
	var evidence []string
	if left.QualifiedName != "" && left.QualifiedName == right.QualifiedName {
		evidence = append(evidence, "qualified_declaration")
	}
	if left.Owner != "" && left.Owner == right.Owner && left.Name == right.Name {
		evidence = append(evidence, "owner_and_name")
	}
	if left.Signature != "" && left.Signature == right.Signature {
		evidence = append(evidence, "signature")
	}
	if sourceLineageEqual(left.Source, right.Source) {
		evidence = append(evidence, "source_content_and_range")
	}
	if left.Name == right.Name {
		evidence = append(evidence, "same_name")
	}
	return uniqueStrings(evidence)
}

func sourceLineageEqual(left, right model.SourceRef) bool {
	return left.ContentHash == right.ContentHash && left.Range == right.Range
}

func changedSymbolFields(left, right DiffSymbol) []string {
	var fields []string
	if left.Kind != right.Kind {
		fields = append(fields, "kind")
	}
	if left.Name != right.Name {
		fields = append(fields, "name")
	}
	if left.QualifiedName != right.QualifiedName {
		fields = append(fields, "qualified_name")
	}
	if left.Owner != right.Owner {
		fields = append(fields, "owner")
	}
	if left.Signature != right.Signature {
		fields = append(fields, "signature")
	}
	if left.SemanticHash != "" && right.SemanticHash != "" && left.SemanticHash != right.SemanticHash {
		fields = append(fields, "semantic_hash")
	}
	sort.Strings(fields)
	return fields
}

func compareRelations(left, right *SemanticSnapshot, leftToRight, rightToLeft map[string]string, leftScope, rightScope DiffScope, result *SemanticDiff) error {
	leftFacts := make([]SemanticFact, 0, len(left.Relations))
	rightFacts := make([]SemanticFact, 0, len(right.Relations))
	leftIDs, rightIDs := make(map[string]string), make(map[string]string)
	leftSymbols := make(map[string]DiffSymbol, len(left.Symbols))
	for _, symbol := range left.Symbols {
		leftSymbols[symbol.ID] = symbol
	}
	for _, relation := range left.Relations {
		if _, sourceOK := leftToRight[relation.SourceSymbolID]; !sourceOK {
			result.UncertainRelations = append(result.UncertainRelations, relation)
			continue
		}
		if relation.TargetSymbolID != "" {
			if _, targetOK := leftToRight[relation.TargetSymbolID]; !targetOK {
				result.UncertainRelations = append(result.UncertainRelations, relation)
				continue
			}
		}
		sourceKey := SymbolFactKey(leftSymbols[relation.SourceSymbolID])
		targetKey := ""
		if relation.TargetSymbolID != "" {
			targetKey = SymbolFactKey(leftSymbols[relation.TargetSymbolID])
		}
		key := RelationFactKey(relation.Kind, sourceKey, targetKey)
		if _, duplicate := leftIDs[key]; duplicate {
			return fmt.Errorf("relation identity %q is not unique: %w", key, ErrInvalidSnapshotDiff)
		}
		leftFacts = append(leftFacts, SemanticFact{ID: relation.ID, Key: key, SemanticHash: relation.SemanticHash})
		leftIDs[key] = relation.ID
	}
	for _, relation := range right.Relations {
		leftSource, sourceOK := rightToLeft[relation.SourceSymbolID]
		leftTarget, targetOK := relation.TargetSymbolID, relation.TargetSymbolID == ""
		if relation.TargetSymbolID != "" {
			leftTarget, targetOK = rightToLeft[relation.TargetSymbolID]
		}
		if !sourceOK || !targetOK {
			result.UncertainRelations = append(result.UncertainRelations, relation)
			continue
		}
		sourceKey := SymbolFactKey(leftSymbols[leftSource])
		targetKey := ""
		if leftTarget != "" {
			targetKey = SymbolFactKey(leftSymbols[leftTarget])
		}
		key := RelationFactKey(relation.Kind, sourceKey, targetKey)
		if _, duplicate := rightIDs[key]; duplicate {
			return fmt.Errorf("relation identity %q is not unique: %w", key, ErrInvalidSnapshotDiff)
		}
		rightFacts = append(rightFacts, SemanticFact{ID: relation.ID, Key: key, SemanticHash: relation.SemanticHash})
		rightIDs[key] = relation.ID
	}
	result.Relations = compareFacts(leftFacts, rightFacts, leftScope, rightScope)
	for i := range result.Relations.Added {
		result.Relations.Added[i].ID = rightIDs[result.Relations.Added[i].Key]
	}
	for i := range result.Relations.Removed {
		result.Relations.Removed[i].ID = leftIDs[result.Relations.Removed[i].Key]
	}
	for i := range result.Relations.Changed {
		result.Relations.Changed[i].Before.ID = leftIDs[result.Relations.Changed[i].Before.Key]
		result.Relations.Changed[i].After.ID = rightIDs[result.Relations.Changed[i].After.Key]
	}
	return nil
}

// RelationFactKey uses stable symbol keys so consumers can refer to an edge
// across generations without depending on provider-local symbol IDs.
func RelationFactKey(kind, sourceKey, targetKey string) string {
	return "relation/" + kind + "/" + sourceKey + "/" + targetKey
}

func compareFacts(left, right []SemanticFact, leftScope, rightScope DiffScope) FactDelta {
	leftByKey, rightByKey := make(map[string]SemanticFact, len(left)), make(map[string]SemanticFact, len(right))
	for _, fact := range left {
		leftByKey[fact.Key] = fact
	}
	for _, fact := range right {
		rightByKey[fact.Key] = fact
	}
	var delta FactDelta
	for key, before := range leftByKey {
		after, exists := rightByKey[key]
		if !exists {
			item := DiffFact{ID: before.ID, Key: key, SemanticHash: before.SemanticHash}
			if rightScope == DiffScopeComplete {
				delta.Removed = append(delta.Removed, item)
			} else {
				delta.Uncertain = append(delta.Uncertain, item)
			}
			continue
		}
		if before.SemanticHash != "" && after.SemanticHash != "" && before.SemanticHash != after.SemanticHash {
			delta.Changed = append(delta.Changed, FactChange{
				Before: DiffFact{ID: before.ID, Key: key, SemanticHash: before.SemanticHash},
				After:  DiffFact{ID: after.ID, Key: key, SemanticHash: after.SemanticHash},
			})
		}
	}
	for key, after := range rightByKey {
		if _, exists := leftByKey[key]; exists {
			continue
		}
		item := DiffFact{ID: after.ID, Key: key, SemanticHash: after.SemanticHash}
		if leftScope == DiffScopeComplete {
			delta.Added = append(delta.Added, item)
		} else {
			delta.Uncertain = append(delta.Uncertain, item)
		}
	}
	sort.Slice(delta.Added, func(i, j int) bool { return delta.Added[i].Key < delta.Added[j].Key })
	sort.Slice(delta.Removed, func(i, j int) bool { return delta.Removed[i].Key < delta.Removed[j].Key })
	sort.Slice(delta.Uncertain, func(i, j int) bool { return delta.Uncertain[i].Key < delta.Uncertain[j].Key })
	sort.Slice(delta.Changed, func(i, j int) bool { return delta.Changed[i].Before.Key < delta.Changed[j].Before.Key })
	return delta
}

func compareConstraints(left, right *SemanticSnapshot, result *SemanticDiff) error {
	leftByID, rightByID := make(map[model.UUID]Version, len(left.Constraints)), make(map[model.UUID]Version, len(right.Constraints))
	for _, record := range left.Constraints {
		leftByID[record.RecordID] = record
	}
	for _, record := range right.Constraints {
		rightByID[record.RecordID] = record
	}
	for id, before := range leftByID {
		after, exists := rightByID[id]
		if !exists {
			if right.Scope == DiffScopeComplete {
				evaluation, err := EvaluateDecisionStatus(before, left)
				if err != nil {
					return err
				}
				result.Constraints.Removed = append(result.Constraints.Removed, evaluation)
			}
			continue
		}
		if before.Version == after.Version && !sameImmutableVersion(before, after) {
			return fmt.Errorf("record %s version %d changed in place: %w", id, before.Version, ErrInvalidSnapshotDiff)
		}
		beforeState, err := EvaluateDecisionStatus(before, left)
		if err != nil {
			return err
		}
		afterState, err := EvaluateDecisionStatus(after, right)
		if err != nil {
			return err
		}
		if before.Version != after.Version || beforeState.EffectiveStatus != afterState.EffectiveStatus || beforeState.ProvenanceStatus != afterState.ProvenanceStatus {
			result.Constraints.Changed = append(result.Constraints.Changed, DecisionChange{Before: beforeState, After: afterState})
		}
	}
	for id, after := range rightByID {
		if _, exists := leftByID[id]; exists || left.Scope != DiffScopeComplete {
			continue
		}
		evaluation, err := EvaluateDecisionStatus(after, right)
		if err != nil {
			return err
		}
		result.Constraints.Added = append(result.Constraints.Added, evaluation)
	}
	sort.Slice(result.Constraints.Added, func(i, j int) bool {
		return result.Constraints.Added[i].RecordID < result.Constraints.Added[j].RecordID
	})
	sort.Slice(result.Constraints.Removed, func(i, j int) bool {
		return result.Constraints.Removed[i].RecordID < result.Constraints.Removed[j].RecordID
	})
	sort.Slice(result.Constraints.Changed, func(i, j int) bool {
		return result.Constraints.Changed[i].Before.RecordID < result.Constraints.Changed[j].Before.RecordID
	})
	return nil
}

func sameImmutableVersion(left, right Version) bool {
	if left.RecordID != right.RecordID || left.Version != right.Version || left.Kind != right.Kind || left.DeclaredStatus != right.DeclaredStatus || left.PayloadHash != right.PayloadHash || left.ActorID != right.ActorID || left.Reason != right.Reason || left.CreatedAt != right.CreatedAt || len(left.SourceRefs) != len(right.SourceRefs) {
		return false
	}
	for index := range left.SourceRefs {
		if left.SourceRefs[index] != right.SourceRefs[index] {
			return false
		}
	}
	return true
}

func validateDecisionRecord(record Version) error {
	if _, err := model.ParseUUID(string(record.RecordID)); err != nil || record.Version < 1 || record.Version > model.MaxSafeInteger || !record.Kind.Valid() || !record.DeclaredStatus.ValidDeclared() {
		return ErrInvalidSnapshotDiff
	}
	if _, err := model.ParseHash(string(record.PayloadHash)); err != nil {
		return ErrInvalidSnapshotDiff
	}
	if len(record.SourceRefs) > 0 {
		if ValidateSourceRefs(record.SourceRefs[0].WorktreeID, record.SourceRefs) != nil {
			return ErrInvalidSnapshotDiff
		}
	}
	return nil
}

// EvaluateDecisionStatus derives current approval from exact retained source
// references. It never writes back to the immutable Version or reanchors refs.
func EvaluateDecisionStatus(record Version, current *SemanticSnapshot) (DecisionEvaluation, error) {
	if err := validateDecisionRecord(record); err != nil {
		return DecisionEvaluation{}, fmt.Errorf("decision record is invalid: %w", ErrInvalidSnapshotDiff)
	}
	evaluation := DecisionEvaluation{RecordID: record.RecordID, Version: record.Version, DeclaredStatus: record.DeclaredStatus, EffectiveStatus: record.DeclaredStatus, ProvenanceStatus: ProvenanceUnverified}
	if record.DeclaredStatus != StatusApproved {
		return evaluation, nil
	}
	if current == nil || current.Availability != SnapshotRetained {
		if current != nil {
			switch current.Availability {
			case SnapshotExpired:
				evaluation.ProvenanceStatus = ProvenanceSnapshotExpired
			case SnapshotDenied:
				evaluation.ProvenanceStatus = ProvenanceAccessDenied
			}
		}
		evaluation.EffectiveStatus = EffectiveStatus(record.DeclaredStatus, evaluation.ProvenanceStatus)
		return evaluation, nil
	}
	if err := current.validate(); err != nil {
		return DecisionEvaluation{}, err
	}
	if current.Scope != DiffScopeComplete || len(record.SourceRefs) == 0 {
		evaluation.ProvenanceStatus = ProvenanceUnverified
		evaluation.EffectiveStatus = StatusNeedsReview
		return evaluation, nil
	}
	for _, ref := range record.SourceRefs {
		if ref.WorktreeID != current.WorktreeID || ref.ProfileID != current.ProfileID {
			evaluation.ProvenanceStatus = ProvenanceSourceChanged
			evaluation.EffectiveStatus = StatusNeedsReview
			return evaluation, nil
		}
		found := false
		for _, source := range current.SourceEvidence {
			if source.WorktreeID == current.WorktreeID && source.ProfileID == current.ProfileID && source.GenerationID == current.GenerationID && source.Path == ref.Path && source.ContentHash == ref.ContentHash && source.Range == ref.Range {
				found = true
				break
			}
		}
		if !found {
			evaluation.ProvenanceStatus = ProvenanceSourceChanged
			evaluation.EffectiveStatus = StatusNeedsReview
			return evaluation, nil
		}
	}
	evaluation.ProvenanceStatus = ProvenanceRetainedSnapshot
	evaluation.EffectiveStatus = StatusApproved
	return evaluation, nil
}

func findAffectedConsumers(left, right *SemanticSnapshot, result *SemanticDiff) []AffectedConsumer {
	touched := make(map[string]struct{})
	for _, change := range result.ChangedSymbols {
		touched[symbolFactKey(change.Before)] = struct{}{}
		touched[symbolFactKey(change.After)] = struct{}{}
		touched[change.Before.ID] = struct{}{}
		touched[change.After.ID] = struct{}{}
	}
	for _, symbol := range result.AddedSymbols {
		touched[symbolFactKey(symbol)] = struct{}{}
		touched[symbol.ID] = struct{}{}
	}
	for _, symbol := range result.RemovedSymbols {
		touched[symbolFactKey(symbol)] = struct{}{}
		touched[symbol.ID] = struct{}{}
	}
	for _, fact := range result.Relations.Added {
		touched[fact.Key] = struct{}{}
		touched[fact.ID] = struct{}{}
	}
	for _, fact := range result.Relations.Removed {
		touched[fact.Key] = struct{}{}
		touched[fact.ID] = struct{}{}
	}
	for _, change := range result.Relations.Changed {
		touched[change.Before.Key] = struct{}{}
		touched[change.After.Key] = struct{}{}
		touched[change.Before.ID] = struct{}{}
		touched[change.After.ID] = struct{}{}
	}
	for _, fact := range result.Contracts.Added {
		touched[fact.Key] = struct{}{}
	}
	for _, fact := range result.Contracts.Removed {
		touched[fact.Key] = struct{}{}
	}
	for _, change := range result.Contracts.Changed {
		touched[change.Before.Key] = struct{}{}
	}
	for _, decision := range result.Constraints.Added {
		touched["decision/"+string(decision.RecordID)] = struct{}{}
	}
	for _, decision := range result.Constraints.Removed {
		touched["decision/"+string(decision.RecordID)] = struct{}{}
	}
	for _, change := range result.Constraints.Changed {
		touched["decision/"+string(change.Before.RecordID)] = struct{}{}
	}
	byID := make(map[string]AffectedConsumer)
	for _, consumers := range [][]DiffConsumer{left.Consumers, right.Consumers} {
		for _, consumer := range consumers {
			var dependencies []string
			for _, key := range consumer.DependencyKeys {
				if _, affected := touched[key]; affected {
					dependencies = append(dependencies, key)
				}
			}
			if len(dependencies) == 0 {
				continue
			}
			sort.Strings(dependencies)
			byID[consumer.ID] = AffectedConsumer{ID: consumer.ID, Name: consumer.Name, DependencyKeys: uniqueStrings(dependencies)}
		}
	}
	consumers := make([]AffectedConsumer, 0, len(byID))
	for _, consumer := range byID {
		consumers = append(consumers, consumer)
	}
	sort.Slice(consumers, func(i, j int) bool { return consumers[i].ID < consumers[j].ID })
	return consumers
}

// SymbolFactKey is stable across generations when qualified identity is known.
func SymbolFactKey(symbol DiffSymbol) string {
	return symbolFactKey(symbol)
}

func symbolFactKey(symbol DiffSymbol) string {
	if symbol.QualifiedName != "" {
		return "symbol/" + symbol.Kind + "/" + symbol.QualifiedName
	}
	if symbol.Owner != "" {
		return "symbol/" + symbol.Kind + "/" + symbol.Owner + "/" + symbol.Name
	}
	return "symbol/" + symbol.Kind + "/" + symbol.Name
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func sortSemanticDiff(result *SemanticDiff) {
	sort.Slice(result.SymbolMappings, func(i, j int) bool {
		if result.SymbolMappings[i].LeftID != result.SymbolMappings[j].LeftID {
			return result.SymbolMappings[i].LeftID < result.SymbolMappings[j].LeftID
		}
		return result.SymbolMappings[i].RightID < result.SymbolMappings[j].RightID
	})
	sort.Slice(result.AddedSymbols, func(i, j int) bool { return result.AddedSymbols[i].ID < result.AddedSymbols[j].ID })
	sort.Slice(result.RemovedSymbols, func(i, j int) bool { return result.RemovedSymbols[i].ID < result.RemovedSymbols[j].ID })
	sort.Slice(result.UncertainSymbols, func(i, j int) bool { return result.UncertainSymbols[i].ID < result.UncertainSymbols[j].ID })
	sort.Slice(result.ChangedSymbols, func(i, j int) bool { return result.ChangedSymbols[i].Before.ID < result.ChangedSymbols[j].Before.ID })
}
