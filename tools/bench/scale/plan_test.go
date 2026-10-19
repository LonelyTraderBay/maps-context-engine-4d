package scale

import (
	"errors"
	"testing"
)

func TestCohortPlansHaveIndependentCountAndByteOracles(t *testing.T) {
	tests := []struct {
		cohort        Cohort
		maxFiles      int64
		maxSourceByte int64
	}{
		{CohortS, 1_000, 20 * MiB},
		{CohortM, 10_000, 200 * MiB},
		{CohortL, 100_000, 4 * GiB},
		{CohortXL, 1_000_000, 32 * GiB},
	}
	for _, test := range tests {
		for _, mode := range []Mode{ModeCountOnly, ModeByteOnly, ModeCombined} {
			t.Run(string(test.cohort)+"/"+string(mode), func(t *testing.T) {
				plan, err := NewPlan(test.cohort, mode)
				if err != nil {
					t.Fatal(err)
				}
				if err := plan.Validate(); err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				wantFiles, wantBytes := test.maxFiles, test.maxSourceByte
				if mode == ModeCountOnly {
					wantBytes = 0
				}
				if mode == ModeByteOnly {
					wantFiles = ceilDiv(wantBytes, DefaultMaxFileBytes)
				}
				if plan.FileCount != wantFiles || plan.SourceBytes != wantBytes {
					t.Fatalf("count/bytes=%d/%d; want %d/%d", plan.FileCount, plan.SourceBytes, wantFiles, wantBytes)
				}
				if plan.SourceBytesMeasured {
					t.Fatal("planned source bytes must not be labeled as measured")
				}
				base := plan.SourceBytes / plan.FileCount
				remainder := plan.SourceBytes % plan.FileCount
				observedBytes := base*plan.FileCount + remainder
				if err := plan.VerifyOracle(plan.FileCount, observedBytes); err != nil {
					t.Fatalf("independent count/byte oracle failed: %v", err)
				}
				maxFile := base
				if remainder > 0 {
					maxFile++
				}
				if maxFile > plan.Limits.MaxFileBytes {
					t.Fatalf("distributed source has file=%d above cap=%d", maxFile, plan.Limits.MaxFileBytes)
				}
				first, err := plan.FileBytes(0)
				if err != nil || first != base+boolInt(remainder > 0) {
					t.Fatalf("first file bytes=%d err=%v", first, err)
				}
			})
		}
	}
}

func TestXLV1PlanCarriesSealedVerificationLimits(t *testing.T) {
	plan, err := NewPlan(CohortXL, ModeCombined)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FileCount != 1_000_000 || plan.SourceBytes != 32*GiB || plan.Profiles != 1 || plan.Worktrees != 1 {
		t.Fatalf("XL-V1 envelope changed: %+v", plan)
	}
	if plan.Limits.CacheBytes != 384*GiB || plan.Limits.RSSBudgetBytes != 8*GiB ||
		plan.Limits.VolumeReserve != 2*GiB || plan.Limits.VolumeWarning != 4*GiB || plan.Limits.IndexDeadlineSecs != 7200 {
		t.Fatalf("sealed XL-V1 config changed: %+v", plan.Limits)
	}
	if plan.ManifestUpperBound <= 0 || plan.ManifestUpperBound >= plan.Limits.MaxManifestBytes {
		t.Fatalf("manifest upper bound is invalid: %d", plan.ManifestUpperBound)
	}
}

func TestPlanRejectsUnsupportedOrIncorrectOracles(t *testing.T) {
	plan, err := NewPlan(CohortXL, ModeCombined)
	if err != nil {
		t.Fatal(err)
	}
	plan.FileCount++
	if err := plan.Validate(); !errors.Is(err, ErrUnsupportedScale) {
		t.Fatalf("over-cap file count error=%v, want ErrUnsupportedScale", err)
	}

	plan, err = NewPlan(CohortM, ModeCombined)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.VerifyOracle(plan.FileCount-1, plan.SourceBytes); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("count oracle error=%v, want ErrInvalidPlan", err)
	}
	if _, err := NewPlan(CohortXL, Mode("unbounded")); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("unknown workload mode error=%v, want ErrInvalidPlan", err)
	}
}
