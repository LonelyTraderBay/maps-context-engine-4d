package compare

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type validatedUsage struct {
	groups               map[taskRunKey]map[Variant]*variantRun
	completeGroupCount   int
	incompleteGroupCount int
	missingGroupCount    int
	uniqueCostSamples    map[string]CostObservation
}

func validateImportedUsage(suite Suite, imported importedUsage) (validatedUsage, error) {
	if err := validateHeader(imported.header, suite); err != nil {
		return validatedUsage{}, err
	}
	tasks := make(map[string]Task, len(suite.Tasks))
	for _, task := range suite.Tasks {
		tasks[task.ID] = task
	}
	repositories := make(map[string]Repository, len(suite.Repositories))
	for _, repository := range suite.Repositories {
		repositories[repository.ID] = repository
	}
	validated := validatedUsage{
		groups:            make(map[taskRunKey]map[Variant]*variantRun),
		uniqueCostSamples: make(map[string]CostObservation, len(imported.costs)),
	}
	seenAttempts := make(map[string]struct{}, len(imported.attempts))
	for _, attempt := range imported.attempts {
		if err := validateAttempt(attempt, imported.header, tasks, repositories); err != nil {
			return validatedUsage{}, err
		}
		identity := strings.Join([]string{attempt.TaskID, strconv.Itoa(attempt.Repeat), string(attempt.Variant), strconv.Itoa(attempt.Attempt)}, "\x00")
		if _, exists := seenAttempts[identity]; exists {
			return validatedUsage{}, fmt.Errorf("duplicate attempt identity for task %q repeat %d variant %s: %w", attempt.TaskID, attempt.Repeat, attempt.Variant, ErrInvalidUsage)
		}
		seenAttempts[identity] = struct{}{}
		key := taskRunKey{task: attempt.TaskID, repeat: attempt.Repeat}
		if validated.groups[key] == nil {
			validated.groups[key] = make(map[Variant]*variantRun, len(variants))
		}
		group := validated.groups[key]
		variant := group[attempt.Variant]
		if variant == nil {
			variant = &variantRun{order: attempt.OrderIndex}
			group[attempt.Variant] = variant
		} else if variant.order != attempt.OrderIndex {
			return validatedUsage{}, fmt.Errorf("task %q repeat %d variant %s changed order index between attempts: %w", attempt.TaskID, attempt.Repeat, attempt.Variant, ErrInvalidUsage)
		}
		variant.attempts = append(variant.attempts, attempt)
	}

	for key, group := range validated.groups {
		orderSeen := make(map[int]Variant, len(group))
		for variant, observations := range group {
			if prior, exists := orderSeen[observations.order]; exists {
				return validatedUsage{}, fmt.Errorf("task %q repeat %d variants %s and %s share order index %d: %w", key.task, key.repeat, prior, variant, observations.order, ErrInvalidUsage)
			}
			orderSeen[observations.order] = variant
			sort.Slice(observations.attempts, func(i, j int) bool { return observations.attempts[i].Attempt < observations.attempts[j].Attempt })
			acceptedCount := 0
			for index, attempt := range observations.attempts {
				if attempt.Attempt != index+1 {
					return validatedUsage{}, fmt.Errorf("task %q repeat %d variant %s attempt sequence has a gap: %w", key.task, key.repeat, variant, ErrInvalidUsage)
				}
				if attempt.Accepted {
					acceptedCount++
					if index != len(observations.attempts)-1 {
						return validatedUsage{}, fmt.Errorf("accepted result has a later retry for task %q variant %s: %w", key.task, variant, ErrInvalidUsage)
					}
				}
			}
			if acceptedCount > 1 {
				return validatedUsage{}, fmt.Errorf("multiple accepted attempts for task %q variant %s: %w", key.task, variant, ErrInvalidUsage)
			}
		}
		if len(group) == len(variants) {
			validated.completeGroupCount++
			want := VariantOrderFor(imported.header.Seed, key.task, key.repeat)
			for order, variant := range want {
				if observed := group[variant]; observed == nil || observed.order != order {
					return validatedUsage{}, fmt.Errorf("task %q repeat %d order does not match seeded shuffle: %w", key.task, key.repeat, ErrInvalidUsage)
				}
			}
		} else {
			validated.incompleteGroupCount++
		}
	}
	expectedGroups := len(suite.Tasks) * imported.header.RepeatCount
	validated.missingGroupCount = expectedGroups - len(validated.groups)

	for _, cost := range imported.costs {
		if err := validateCost(cost, imported.header, repositories); err != nil {
			return validatedUsage{}, err
		}
		identity := strings.Join([]string{cost.RepositoryID, string(cost.Variant), cost.Stage, cost.CacheState, cost.SampleID}, "\x00")
		if _, exists := validated.uniqueCostSamples[identity]; exists {
			return validatedUsage{}, fmt.Errorf("duplicate cost sample %q: %w", cost.SampleID, ErrInvalidUsage)
		}
		validated.uniqueCostSamples[identity] = cost
	}
	return validated, nil
}

// VariantOrderFor returns a cross-platform deterministic Fisher-Yates shuffle.
// The algorithm and seed inputs are fixed so independent hosts can audit order.
func VariantOrderFor(seed, taskID string, repeat int) [3]Variant {
	order := [3]Variant{VariantA, VariantB, VariantC}
	digest := sha256.Sum256([]byte(seed + "\x00" + taskID + "\x00" + strconv.Itoa(repeat)))
	state := binary.LittleEndian.Uint64(digest[:8])
	for index := len(order) - 1; index > 0; index-- {
		bound := uint64(index + 1)
		threshold := -bound % bound
		var sample uint64
		for {
			state, sample = splitMix64(state)
			if sample >= threshold {
				break
			}
		}
		other := int(sample % bound)
		order[index], order[other] = order[other], order[index]
	}
	return order
}

func splitMix64(state uint64) (uint64, uint64) {
	state += 0x9e3779b97f4a7c15
	value := state
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	value ^= value >> 31
	return state, value
}
