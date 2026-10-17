package contextpack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	MaxResponseItems      = 512
	maxGapSourceRefInputs = 100000
)

var ErrInvalidBudgetInput = errors.New("invalid context budget input")

type BudgetCandidate struct {
	ID            string
	Required      bool
	Category      SliceCategory
	PayloadBytes  int64
	PayloadDigest model.Hash
}

type BudgetOptions struct {
	// BaseBytes is the delivered byte size of the response with an empty items array.
	BaseBytes          int64
	MaxBytes           int64
	MaxItems           int
	DeliveryMultiplier int64
}

type BudgetSelection struct {
	Candidates      []BudgetCandidate
	RequiredPending *int
	OptionalPending *int
	Truncated       bool
	LimitReason     string
}

type GapSummary struct {
	Gaps              []Gap
	MetadataTruncated bool
	DetailsOmitted    int
}

// SelectBudget admits whole JSON items in deterministic priority order.
// BaseBytes already includes the complete response envelope and its empty items array.
func SelectBudget(ctx context.Context, candidates []BudgetCandidate, options BudgetOptions) (BudgetSelection, error) {
	if ctx == nil || options.BaseBytes < 0 || options.BaseBytes > model.MaxSafeInteger ||
		options.MaxBytes < 1 || options.MaxBytes > model.MaxSafeInteger ||
		options.MaxItems < 1 || options.MaxItems > MaxResponseItems ||
		options.DeliveryMultiplier < 1 || options.DeliveryMultiplier > 2 {
		return BudgetSelection{}, fmt.Errorf("%w: invalid limits", ErrInvalidBudgetInput)
	}
	if err := ctx.Err(); err != nil {
		return BudgetSelection{}, err
	}

	unique, err := uniqueBudgetCandidates(ctx, candidates)
	if err != nil {
		return BudgetSelection{}, err
	}
	sort.Slice(unique, func(i, j int) bool {
		left, right := unique[i], unique[j]
		if left.Required != right.Required {
			return left.Required
		}
		leftRank, rightRank := sliceCategoryRank(left.Category), sliceCategoryRank(right.Category)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return left.PayloadDigest < right.PayloadDigest
	})
	if err := ctx.Err(); err != nil {
		return BudgetSelection{}, err
	}

	selection := BudgetSelection{Candidates: make([]BudgetCandidate, 0, min(len(unique), options.MaxItems))}
	var requiredOmitted, optionalOmitted int
	usedBytes := options.BaseBytes
	for _, candidate := range unique {
		if err := ctx.Err(); err != nil {
			return BudgetSelection{}, err
		}
		itemBytes, ok := multiplyBudgetBytes(candidate.PayloadBytes, options.DeliveryMultiplier)
		if !ok {
			return BudgetSelection{}, fmt.Errorf("%w: candidate byte count overflow", ErrInvalidBudgetInput)
		}
		if len(selection.Candidates) > 0 {
			itemBytes += options.DeliveryMultiplier // one JSON comma per delivered copy
		}
		fits := len(selection.Candidates) < options.MaxItems && usedBytes <= options.MaxBytes &&
			itemBytes <= options.MaxBytes-usedBytes
		if !fits {
			selection.Truncated = true
			if len(selection.Candidates) >= options.MaxItems {
				selection.LimitReason = "item_count_limit"
			} else if selection.LimitReason == "" {
				selection.LimitReason = "output_byte_budget"
			}
			if candidate.Required {
				requiredOmitted++
			} else {
				optionalOmitted++
			}
			continue
		}
		usedBytes += itemBytes
		selection.Candidates = append(selection.Candidates, candidate)
	}
	if requiredOmitted > 0 {
		selection.RequiredPending = &requiredOmitted
	}
	if optionalOmitted > 0 {
		selection.OptionalPending = &optionalOmitted
	}
	return selection, nil
}

// GroupGaps preserves distinct code/reason pairs while bounding response metadata.
func GroupGaps(ctx context.Context, gaps []Gap, maxGroups int) (GapSummary, error) {
	if ctx == nil || maxGroups < 1 || maxGroups > 64 || len(gaps) > maxGraphGaps {
		return GapSummary{}, fmt.Errorf("%w: invalid gap group limit", ErrInvalidBudgetInput)
	}
	if err := ctx.Err(); err != nil {
		return GapSummary{}, err
	}
	groups := make(map[string]*Gap, len(gaps))
	refs := make(map[string]map[string]model.SourceRef)
	totalSourceRefs := 0
	for _, gap := range gaps {
		if err := ctx.Err(); err != nil {
			return GapSummary{}, err
		}
		if !validID(gap.Code) || !validBoundedData(gap.Reason, 1024) || len(gap.SourceRefs) > maxEvidenceRefs ||
			(gap.Count != nil && (*gap.Count < 0 || int64(*gap.Count) > model.MaxSafeInteger)) {
			return GapSummary{}, fmt.Errorf("%w: invalid gap code or reason", ErrInvalidBudgetInput)
		}
		key := gap.Code + "\x00" + gap.Reason
		group := groups[key]
		if group == nil {
			group = &Gap{Code: gap.Code, Reason: gap.Reason}
			if gap.Count != nil {
				value := *gap.Count
				group.Count = &value
			}
			groups[key] = group
			refs[key] = make(map[string]model.SourceRef)
		} else if group.Count != nil {
			maxCount := int64(^uint(0) >> 1)
			if maxCount > model.MaxSafeInteger {
				maxCount = model.MaxSafeInteger
			}
			if gap.Count == nil || int64(*gap.Count) > maxCount-int64(*group.Count) {
				group.Count = nil
			} else {
				*group.Count += *gap.Count
			}
		}
		for _, source := range gap.SourceRefs {
			totalSourceRefs++
			if totalSourceRefs > maxGapSourceRefInputs {
				return GapSummary{}, fmt.Errorf("%w: gap source references exceed the aggregation work limit", ErrInvalidBudgetInput)
			}
			if err := validateSourceRef(source); err != nil {
				return GapSummary{}, fmt.Errorf("%w: invalid source reference in gap", ErrInvalidBudgetInput)
			}
			refs[key][sourceRefKey(source)] = source
		}
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := ctx.Err(); err != nil {
		return GapSummary{}, err
	}
	summary := GapSummary{Gaps: make([]Gap, 0, min(len(keys), maxGroups))}
	needsSummary := len(keys) > maxGroups
	if !needsSummary {
		for _, key := range keys {
			if len(refs[key]) > 16 {
				needsSummary = true
				break
			}
		}
	}
	keepCount := len(keys)
	if needsSummary && keepCount >= maxGroups {
		keepCount = maxGroups - 1
		summary.MetadataTruncated = true
	}
	for index, key := range keys {
		if err := ctx.Err(); err != nil {
			return GapSummary{}, err
		}
		if index >= keepCount {
			summary.DetailsOmitted += 1 + len(refs[key])
			continue
		}
		group := *groups[key]
		orderedRefs := make([]model.SourceRef, 0, len(refs[key]))
		for _, source := range refs[key] {
			orderedRefs = append(orderedRefs, source)
		}
		sort.Slice(orderedRefs, func(i, j int) bool { return sourceRefLess(orderedRefs[i], orderedRefs[j]) })
		if err := ctx.Err(); err != nil {
			return GapSummary{}, err
		}
		if len(orderedRefs) > 16 {
			summary.MetadataTruncated = true
			summary.DetailsOmitted += len(orderedRefs) - 16
			orderedRefs = orderedRefs[:16]
		}
		group.SourceRefs = orderedRefs
		summary.Gaps = append(summary.Gaps, group)
	}
	if summary.MetadataTruncated {
		summary.Gaps = append(summary.Gaps, Gap{
			Code:   "gap_details_pending",
			Reason: "Additional distinct gap groups or source references remain pending due to the response metadata limit.",
			Count:  &summary.DetailsOmitted,
		})
	}
	return summary, nil
}

func uniqueBudgetCandidates(ctx context.Context, candidates []BudgetCandidate) ([]BudgetCandidate, error) {
	unique := make(map[string]BudgetCandidate, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !utf8.ValidString(candidate.ID) || candidate.ID == "" || len(candidate.ID) > 192 ||
			!validBudgetCategory(candidate.Category) || candidate.PayloadBytes < 2 || candidate.PayloadBytes > model.MaxSafeInteger {
			return nil, fmt.Errorf("%w: candidate identity or payload size is invalid", ErrInvalidBudgetInput)
		}
		if _, err := model.ParseHash(string(candidate.PayloadDigest)); err != nil {
			return nil, fmt.Errorf("%w: candidate payload digest is invalid", ErrInvalidBudgetInput)
		}
		if _, exists := unique[candidate.ID]; !exists {
			unique[candidate.ID] = candidate
			continue
		}
		prior := unique[candidate.ID]
		if prior.PayloadBytes != candidate.PayloadBytes || prior.PayloadDigest != candidate.PayloadDigest {
			return nil, fmt.Errorf("%w: duplicate identity has different payload", ErrInvalidBudgetInput)
		}
		if candidate.Required {
			prior.Required = true
		}
		if sliceCategoryRank(candidate.Category) < sliceCategoryRank(prior.Category) {
			prior.Category = candidate.Category
		}
		unique[candidate.ID] = prior
	}
	result := make([]BudgetCandidate, 0, len(unique))
	for _, candidate := range unique {
		result = append(result, candidate)
	}
	return result, nil
}

func multiplyBudgetBytes(value, multiplier int64) (int64, bool) {
	if value < 0 || multiplier < 1 || value > math.MaxInt64/multiplier {
		return 0, false
	}
	return value * multiplier, true
}

func validBudgetCategory(category SliceCategory) bool {
	switch category {
	case SliceSafetyBoundary, SliceTarget, SliceIntended, SliceObserved, SliceDeclaration,
		SliceSource, SliceReference, SliceDataflow, SliceDependency, SliceContract, SliceTest,
		SliceDocumentation, SliceModule, SliceLexicalCandidate, SliceNeighbor:
		return true
	default:
		return false
	}
}
