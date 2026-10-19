// Package scale defines the finite workload envelopes used by scale acceptance
// fixtures. Plans describe targets; they never claim that a workload ran.
package scale

import (
	"errors"
	"fmt"
)

const (
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30

	DefaultMaxFiles          int64 = 1_000_000
	DefaultMaxFileBytes      int64 = 2 * MiB
	DefaultMaxSourceBytes    int64 = 32 * GiB
	DefaultMaxManifestBytes  int64 = 32 * GiB
	DefaultCacheBytes        int64 = 384 * GiB
	DefaultRSSBudgetBytes    int64 = 8 * GiB
	DefaultVolumeReserve     int64 = 2 * GiB
	DefaultVolumeWarning     int64 = 4 * GiB
	MaxManifestPathBytes     int64 = 4096
	ManifestFixedOverhead    int64 = 512
	ManifestEntryFixedBytes  int64 = 512
	DefaultChurnBasisPoints  int64 = 1000
	DefaultIndexDeadlineSecs int64 = 7200
)

var (
	ErrInvalidPlan      = errors.New("invalid scale workload plan")
	ErrUnsupportedScale = errors.New("scale workload exceeds the sealed XL envelope")
)

type Cohort string

const (
	CohortS  Cohort = "S"
	CohortM  Cohort = "M"
	CohortL  Cohort = "L"
	CohortXL Cohort = "XL"
)

type Mode string

const (
	ModeCountOnly Mode = "count-only"
	ModeByteOnly  Mode = "byte-only"
	ModeCombined  Mode = "combined"
)

// Limits is the sealed verification configuration, separate from interactive
// defaults. Raising one of these values needs a new authorized configuration.
type Limits struct {
	MaxFiles          int64 `json:"max_indexed_files"`
	MaxFileBytes      int64 `json:"max_file_bytes"`
	MaxSourceBytes    int64 `json:"max_source_bytes_per_snapshot"`
	MaxManifestBytes  int64 `json:"max_capture_manifest_bytes"`
	CacheBytes        int64 `json:"max_worktree_cache_bytes"`
	RSSBudgetBytes    int64 `json:"process_tree_rss_budget_bytes"`
	VolumeReserve     int64 `json:"volume_reserve_bytes"`
	VolumeWarning     int64 `json:"volume_warning_bytes"`
	IndexDeadlineSecs int64 `json:"index_timeout_seconds"`
}

func XLV1Limits() Limits {
	return Limits{
		MaxFiles: DefaultMaxFiles, MaxFileBytes: DefaultMaxFileBytes,
		MaxSourceBytes: DefaultMaxSourceBytes, MaxManifestBytes: DefaultMaxManifestBytes,
		CacheBytes: DefaultCacheBytes, RSSBudgetBytes: DefaultRSSBudgetBytes,
		VolumeReserve: DefaultVolumeReserve, VolumeWarning: DefaultVolumeWarning,
		IndexDeadlineSecs: DefaultIndexDeadlineSecs,
	}
}

type Plan struct {
	FormatVersion       int64  `json:"format_version"`
	Kind                string `json:"kind"`
	Cohort              Cohort `json:"cohort"`
	Mode                Mode   `json:"mode"`
	FileCount           int64  `json:"file_count"`
	SourceBytes         int64  `json:"source_bytes_target"`
	MaxFileBytes        int64  `json:"max_file_bytes"`
	Profiles            int64  `json:"profiles"`
	Worktrees           int64  `json:"worktrees"`
	FanoutFilesPerDir   int64  `json:"fanout_files_per_directory"`
	ChurnFiles          int64  `json:"churn_files"`
	ChurnBasisPoints    int64  `json:"churn_basis_points"`
	ManifestUpperBound  int64  `json:"manifest_upper_bound_bytes"`
	SingleProfile       bool   `json:"single_effective_profile"`
	SourceBytesMeasured bool   `json:"source_bytes_measured"`
	Limits              Limits `json:"sealed_limits"`
}

type cohortEnvelope struct {
	files int64
	bytes int64
}

var envelopes = map[Cohort]cohortEnvelope{
	CohortS:  {files: 1_000, bytes: 20 * MiB},
	CohortM:  {files: 10_000, bytes: 200 * MiB},
	CohortL:  {files: 100_000, bytes: 4 * GiB},
	CohortXL: {files: 1_000_000, bytes: 32 * GiB},
}

func NewPlan(cohort Cohort, mode Mode) (Plan, error) {
	envelope, ok := envelopes[cohort]
	if !ok || !validMode(mode) {
		return Plan{}, ErrInvalidPlan
	}
	plan := Plan{
		FormatVersion: 1, Kind: "scale_workload_plan", Cohort: cohort, Mode: mode,
		Profiles: 1, Worktrees: 1, FanoutFilesPerDir: 1_000,
		ChurnBasisPoints: DefaultChurnBasisPoints, SingleProfile: true,
		Limits: XLV1Limits(),
	}
	plan.MaxFileBytes = plan.Limits.MaxFileBytes
	switch mode {
	case ModeCountOnly:
		plan.FileCount = envelope.files
		plan.SourceBytes = 0
	case ModeByteOnly:
		plan.SourceBytes = envelope.bytes
		plan.FileCount = ceilDiv(envelope.bytes, plan.Limits.MaxFileBytes)
	case ModeCombined:
		plan.FileCount = envelope.files
		plan.SourceBytes = envelope.bytes
	}
	plan.ChurnFiles = plan.FileCount * plan.ChurnBasisPoints / 10_000
	if plan.ChurnFiles == 0 && plan.FileCount > 0 {
		plan.ChurnFiles = 1
	}
	upper, ok := manifestUpperBound(plan.FileCount)
	if !ok {
		return Plan{}, ErrUnsupportedScale
	}
	plan.ManifestUpperBound = upper
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (plan Plan) Validate() error {
	envelope, ok := envelopes[plan.Cohort]
	if !ok || !validMode(plan.Mode) || plan.FormatVersion != 1 || plan.Kind != "scale_workload_plan" ||
		plan.FileCount < 1 || plan.SourceBytes < 0 || plan.Profiles < 1 || plan.Worktrees < 1 ||
		plan.FanoutFilesPerDir < 1 || plan.ChurnFiles < 0 || plan.ChurnFiles > plan.FileCount ||
		plan.ChurnBasisPoints < 0 || plan.ChurnBasisPoints > 10_000 || !validLimits(plan.Limits) {
		return ErrInvalidPlan
	}
	if plan.FileCount > plan.Limits.MaxFiles || plan.SourceBytes > plan.Limits.MaxSourceBytes ||
		plan.FileCount > envelope.files || plan.SourceBytes > envelope.bytes {
		return ErrUnsupportedScale
	}
	if (plan.Mode == ModeCountOnly && plan.SourceBytes != 0) ||
		(plan.Mode != ModeCountOnly && plan.SourceBytes == 0) ||
		(plan.Mode == ModeByteOnly && plan.FileCount > ceilDiv(plan.SourceBytes, plan.MaxFileBytes)) ||
		plan.MaxFileBytes != plan.Limits.MaxFileBytes {
		return ErrInvalidPlan
	}
	upper, ok := manifestUpperBound(plan.FileCount)
	if !ok || plan.ManifestUpperBound != upper || upper > plan.Limits.MaxManifestBytes {
		return ErrUnsupportedScale
	}
	if plan.Profiles != 1 || plan.Worktrees != 1 || !plan.SingleProfile {
		return fmt.Errorf("multi-profile/worktree runs require a separate retention admission plan: %w", ErrInvalidPlan)
	}
	return nil
}

// FileBytes distributes a target byte total deterministically without ever
// exceeding the configured per-file cap.
func (plan Plan) FileBytes(index int64) (int64, error) {
	if err := plan.Validate(); err != nil {
		return 0, err
	}
	if index < 0 || index >= plan.FileCount {
		return 0, ErrInvalidPlan
	}
	base := plan.SourceBytes / plan.FileCount
	remainder := plan.SourceBytes % plan.FileCount
	if index < remainder {
		base++
	}
	if base > plan.MaxFileBytes {
		return 0, ErrUnsupportedScale
	}
	return base, nil
}

// VerifyOracle compares independent observed totals with the frozen plan.
func (plan Plan) VerifyOracle(fileCount, sourceBytes int64) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if fileCount != plan.FileCount || sourceBytes != plan.SourceBytes {
		return fmt.Errorf("observed count/bytes %d/%d; expected %d/%d: %w",
			fileCount, sourceBytes, plan.FileCount, plan.SourceBytes, ErrInvalidPlan)
	}
	return nil
}

func validMode(mode Mode) bool {
	return mode == ModeCountOnly || mode == ModeByteOnly || mode == ModeCombined
}

func validLimits(limits Limits) bool {
	return limits.MaxFiles > 0 && limits.MaxFileBytes > 0 && limits.MaxSourceBytes > 0 &&
		limits.MaxManifestBytes > 0 && limits.CacheBytes > 0 && limits.RSSBudgetBytes > 0 &&
		limits.VolumeReserve > 0 && limits.VolumeWarning >= limits.VolumeReserve && limits.IndexDeadlineSecs > 0
}

func ceilDiv(value, divisor int64) int64 {
	return value/divisor + boolInt(value%divisor != 0)
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func manifestUpperBound(fileCount int64) (int64, bool) {
	if fileCount < 0 || fileCount > (int64(^uint64(0)>>1)-ManifestFixedOverhead)/(MaxManifestPathBytes+ManifestEntryFixedBytes) {
		return 0, false
	}
	return ManifestFixedOverhead + fileCount*(MaxManifestPathBytes+ManifestEntryFixedBytes), true
}
