package config

import (
	"fmt"
)

type RuntimeConfig struct {
	SchemaVersion int64                 `json:"schema_version"`
	Core          CoreRuntimeLimits     `json:"core"`
	Explorer      ExplorerRuntimeLimits `json:"explorer"`
}

type CoreRuntimeLimits struct {
	MaxFileBytes              int64 `json:"max_file_bytes"`
	MaxIndexedFiles           int64 `json:"max_indexed_files"`
	MaxSourceBytesPerSnapshot int64 `json:"max_source_bytes_per_snapshot"`
	MaxParallelIndexJobs      int64 `json:"max_parallel_index_jobs"`
	MaxParseWorkers           int64 `json:"max_parse_workers"`
	MaxLPSSessions            int64 `json:"max_lsp_sessions"`
	SQLiteBusyTimeoutMS       int64 `json:"sqlite_busy_timeout_ms"`
	WriterLockWaitMS          int64 `json:"writer_lock_wait_ms"`
	QueryTimeoutMS            int64 `json:"query_timeout_ms"`
	LSPStartupTimeoutMS       int64 `json:"lsp_startup_timeout_ms"`
	RenameTimeoutMS           int64 `json:"rename_timeout_ms"`
	IndexTimeoutMS            int64 `json:"index_timeout_ms"`
	WatchPollIntervalMS       int64 `json:"watch_poll_interval_ms"`
	FullReconcileIntervalMS   int64 `json:"full_reconcile_interval_ms"`
	MaxOutputBytes            int64 `json:"max_output_bytes"`
	MaxGraphNodes             int64 `json:"max_graph_nodes"`
	MaxGraphEdges             int64 `json:"max_graph_edges"`
	MaxGraphDepth             int64 `json:"max_graph_depth"`
	KeepGenerations           int64 `json:"keep_generations"`
	PackTTLSeconds            int64 `json:"pack_ttl_seconds"`
	MaxWorktreeCacheBytes     int64 `json:"max_worktree_cache_bytes"`
	LogFiles                  int64 `json:"log_files"`
	LogFileBytes              int64 `json:"log_file_bytes"`
	MaintenanceWaitMS         int64 `json:"maintenance_wait_ms"`
	RenameResultMaxBytes      int64 `json:"rename_result_max_bytes"`
	RenameResultsMaxBytes     int64 `json:"rename_results_max_bytes"`
	RenameResultTTLSeconds    int64 `json:"rename_result_ttl_seconds"`
	RenameMaxEdits            int64 `json:"rename_max_edits"`
	ResultTTLSeconds          int64 `json:"result_ttl_seconds"`
	QueryResultCacheBytes     int64 `json:"query_result_cache_bytes"`
	MaxRetainedQueryResults   int64 `json:"max_retained_query_results"`
	MaxCaptureManifestBytes   int64 `json:"max_capture_manifest_bytes"`
	VolumeReserveBytes        int64 `json:"volume_reserve_bytes"`
	VolumeWarningBytes        int64 `json:"volume_warning_bytes"`
	ProcessTreeRSSBudgetBytes int64 `json:"process_tree_rss_budget_bytes"`
	ResourceMonitorIntervalMS int64 `json:"resource_monitor_interval_ms"`
}

type ExplorerRuntimeLimits struct {
	GraphDefaultNodes   int64 `json:"graph_default_nodes"`
	GraphDefaultEdges   int64 `json:"graph_default_edges"`
	GraphHardNodes      int64 `json:"graph_hard_nodes"`
	GraphHardEdges      int64 `json:"graph_hard_edges"`
	FlowMaxSteps        int64 `json:"flow_max_steps"`
	FlowMaxEdges        int64 `json:"flow_max_edges"`
	HoverHz             int64 `json:"hover_hz"`
	SessionTTLSeconds   int64 `json:"session_ttl_seconds"`
	BearerTTLSeconds    int64 `json:"bearer_ttl_seconds"`
	AckTimeoutMS        int64 `json:"ack_timeout_ms"`
	DetachTimeoutMS     int64 `json:"detach_timeout_ms"`
	PollVisibleMS       int64 `json:"poll_visible_ms"`
	PollHiddenMS        int64 `json:"poll_hidden_ms"`
	PollMissLimit       int64 `json:"poll_miss_limit"`
	BridgeMessageBytes  int64 `json:"bridge_message_bytes"`
	MaxPreviewSessions  int64 `json:"max_preview_sessions"`
	MapWatchPollMS      int64 `json:"map_watch_poll_ms"`
	MapArtifactMaxBytes int64 `json:"map_artifact_max_bytes"`
	MapPendingMax       int64 `json:"map_pending_max"`
	MapWatchDebounceMS  int64 `json:"map_watch_debounce_ms"`
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		SchemaVersion: 1,
		Core: CoreRuntimeLimits{
			MaxFileBytes: 2_097_152, MaxIndexedFiles: 1_000_000,
			MaxSourceBytesPerSnapshot: 34_359_738_368, MaxParallelIndexJobs: 2,
			MaxParseWorkers: 0, MaxLPSSessions: 1, SQLiteBusyTimeoutMS: 5_000,
			WriterLockWaitMS: 5_000, QueryTimeoutMS: 10_000, LSPStartupTimeoutMS: 30_000,
			RenameTimeoutMS: 30_000, IndexTimeoutMS: 600_000, WatchPollIntervalMS: 2_000,
			FullReconcileIntervalMS: 60_000, MaxOutputBytes: 24_576, MaxGraphNodes: 2_000,
			MaxGraphEdges: 10_000, MaxGraphDepth: 8, KeepGenerations: 2, PackTTLSeconds: 900,
			MaxWorktreeCacheBytes: 412_316_860_416, LogFiles: 5, LogFileBytes: 10_485_760,
			MaintenanceWaitMS: 5_000, RenameResultMaxBytes: 33_554_432,
			RenameResultsMaxBytes: 134_217_728, RenameResultTTLSeconds: 900,
			RenameMaxEdits: 10_000, ResultTTLSeconds: 900, QueryResultCacheBytes: 33_554_432,
			MaxRetainedQueryResults: 32, MaxCaptureManifestBytes: 34_359_738_368,
			VolumeReserveBytes: 2_147_483_648, VolumeWarningBytes: 4_294_967_296,
			ProcessTreeRSSBudgetBytes: 8_589_934_592, ResourceMonitorIntervalMS: 250,
		},
		Explorer: ExplorerRuntimeLimits{
			GraphDefaultNodes: 200, GraphDefaultEdges: 500, GraphHardNodes: 500,
			GraphHardEdges: 1_500, FlowMaxSteps: 100, FlowMaxEdges: 300, HoverHz: 15,
			SessionTTLSeconds: 1_800, BearerTTLSeconds: 1_800, AckTimeoutMS: 3_000,
			DetachTimeoutMS: 1_000, PollVisibleMS: 2_000, PollHiddenMS: 5_000,
			PollMissLimit: 2, BridgeMessageBytes: 16_384, MaxPreviewSessions: 4,
			MapWatchPollMS: 2_000, MapArtifactMaxBytes: 67_108_864, MapPendingMax: 1,
			MapWatchDebounceMS: 300,
		},
	}
}

func (config RuntimeConfig) ValidateEffective() error {
	if config.SchemaVersion != 1 {
		return fmt.Errorf("%w: unsupported schema version", ErrInvalidConfig)
	}
	core := config.Core
	if core.MaxFileBytes > core.MaxSourceBytesPerSnapshot || core.MaxCaptureManifestBytes > core.MaxSourceBytesPerSnapshot {
		return fmt.Errorf("%w: file and manifest caps must not exceed snapshot source bytes", ErrInvalidConfig)
	}
	if core.FullReconcileIntervalMS < core.WatchPollIntervalMS {
		return fmt.Errorf("%w: full reconcile interval must be at least the watch poll interval", ErrInvalidConfig)
	}
	if core.RenameResultMaxBytes > core.RenameResultsMaxBytes || core.RenameResultsMaxBytes > core.MaxWorktreeCacheBytes {
		return fmt.Errorf("%w: rename result caps exceed their enclosing budgets", ErrInvalidConfig)
	}
	if core.VolumeWarningBytes < core.VolumeReserveBytes {
		return fmt.Errorf("%w: volume warning threshold must be at least its reserve", ErrInvalidConfig)
	}
	if config.Explorer.GraphDefaultNodes > config.Explorer.GraphHardNodes || config.Explorer.GraphDefaultEdges > config.Explorer.GraphHardEdges {
		return fmt.Errorf("%w: graph defaults exceed hard limits", ErrInvalidConfig)
	}
	if core.MaxParseWorkers < 1 {
		return fmt.Errorf("%w: effective parse worker count must be positive", ErrInvalidConfig)
	}
	return nil
}

var processOverrideAllowed = map[string]bool{
	"max_file_bytes": true, "max_indexed_files": true, "max_source_bytes_per_snapshot": true,
	"max_parallel_index_jobs": true, "max_parse_workers": true, "max_lsp_sessions": true,
	"sqlite_busy_timeout_ms": true, "writer_lock_wait_ms": true, "query_timeout_ms": true,
	"lsp_startup_timeout_ms": true, "rename_timeout_ms": true, "index_timeout_ms": true,
	"watch_poll_interval_ms": true, "full_reconcile_interval_ms": true, "max_output_bytes": true,
	"max_graph_nodes": true, "max_graph_edges": true, "max_graph_depth": true,
	"keep_generations": true, "pack_ttl_seconds": true, "log_files": true,
	"log_file_bytes": true, "maintenance_wait_ms": true, "rename_result_max_bytes": true,
	"rename_results_max_bytes": true, "rename_result_ttl_seconds": true, "rename_max_edits": true,
	"result_ttl_seconds": true, "query_result_cache_bytes": true, "max_retained_query_results": true,
	"max_capture_manifest_bytes": true, "process_tree_rss_budget_bytes": true,
	"resource_monitor_interval_ms": true,
}
