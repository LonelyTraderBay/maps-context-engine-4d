export type EnvelopeStatus = "ok" | "partial" | "blocked" | "error";

export interface ApiErrorData {
  code: string;
  message: string;
  retryable: boolean;
  suggested_action: string;
}

export interface Freshness {
  status: string;
  source_view: string;
  scope: string;
  buffer_scope: string;
  consistency: string;
}

export interface Pagination {
  truncated: boolean;
  enumeration_complete: boolean;
  returned_count: number;
  required_pending: string[] | null;
  optional_pending: string[] | null;
  next_cursor: string | null;
  limit_reason: string | null;
}

export interface ApiEnvelope<T> {
  schema_version: 1;
  request_id: string;
  status: EnvelopeStatus;
  scope: unknown | null;
  freshness: Freshness;
  readiness: "reviewable" | "needs_context" | "blocked";
  data: T | null;
  pagination: Pagination;
  warnings: unknown[];
  error: ApiErrorData | null;
}

export interface BootstrapData {
  operation: "bootstrap";
  bearer: string;
  expires_at: string;
  capabilities: string[];
}

export interface WorkspaceData {
  operation: "status";
  index_state: "missing" | "published" | "building_with_previous" | "unavailable";
  project_stage: "spec_only" | "mixed" | "implemented_scope" | "unknown";
  scopes: unknown[];
  capabilities: unknown[];
  registered_scopes: unknown[];
}

export interface SourceRef {
  worktree_id: string;
  profile_id: string;
  generation_id: string;
  path: string;
  content_hash: string;
  range: {
    start_byte: number;
    end_byte: number;
  };
}

export type GraphPlane = "intended" | "observed_static" | "observed_runtime" | "annotation";

export interface GraphNode {
  node_id: string;
  kind: string;
  label: string;
  plane: GraphPlane;
  ref: unknown;
  evidence_refs: unknown[];
  source_refs: SourceRef[];
  expandable_count: number | null;
}

export interface GraphEdge {
  edge_id: string;
  from_node_id: string;
  to_node_id: string;
  relation_kind: string;
  resolution_status: "exact_in_supported_scope" | "candidate" | "unresolved";
  evidence_refs: unknown[];
  expand_ref: string | null;
}

export interface GraphData {
  operation: "graph_query";
  nodes: GraphNode[];
  edges: GraphEdge[];
  subgraph_digest: string;
}

export interface SourceData {
  operation: "source_read";
  source_ref: SourceRef;
  content: string;
}

export interface ExplorerApi {
  bootstrap(code: string, signal?: AbortSignal): Promise<ApiEnvelope<BootstrapData>>;
  workspace(bearer: string, signal?: AbortSignal): Promise<ApiEnvelope<WorkspaceData>>;
  graph(bearer: string, signal?: AbortSignal): Promise<ApiEnvelope<GraphData>>;
  readSource(bearer: string, sourceRef: SourceRef, signal?: AbortSignal): Promise<ApiEnvelope<SourceData>>;
}

export class ExplorerRequestError extends Error {
  readonly code: string | null;

  constructor(message: string, code: string | null = null) {
    super(message);
    this.name = "ExplorerRequestError";
    this.code = code;
  }
}
