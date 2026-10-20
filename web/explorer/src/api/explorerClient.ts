import type {
  ApiEnvelope,
  BootstrapData,
  ExplorerApi,
  GraphData,
  SourceData,
  SourceRef,
  WorkspaceData,
} from "./types";
import { ExplorerRequestError } from "./types";
import { sourceRefKey } from "../state/selection";

const API_PREFIX = "/api/explorer/v1";
const REQUEST_TIMEOUT_MS = 15_000;
const GRAPH_NODE_LIMIT = 200;
const GRAPH_EDGE_LIMIT = 500;
const GRAPH_DEPTH = 3;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

type EnvelopeCandidate = Record<string, unknown> & {
  schema_version?: unknown;
  request_id?: unknown;
  status?: unknown;
  error?: unknown;
  data?: unknown;
};
type ErrorCandidate = Record<string, unknown> & { code?: unknown; message?: unknown };
type SourceRefCandidate = Record<string, unknown> & {
  worktree_id?: unknown;
  profile_id?: unknown;
  generation_id?: unknown;
  path?: unknown;
  content_hash?: unknown;
  range?: unknown;
};
type ByteRangeCandidate = Record<string, unknown> & { start_byte?: unknown; end_byte?: unknown };
type GraphDataCandidate = Record<string, unknown> & {
  operation?: unknown;
  subgraph_digest?: unknown;
  nodes?: unknown;
  edges?: unknown;
};
type GraphNodeCandidate = Record<string, unknown> & {
  node_id?: unknown;
  kind?: unknown;
  label?: unknown;
  plane?: unknown;
  ref?: unknown;
  expandable_count?: unknown;
  source_refs?: unknown;
  evidence_refs?: unknown;
};
type GraphEdgeCandidate = Record<string, unknown> & {
  edge_id?: unknown;
  from_node_id?: unknown;
  to_node_id?: unknown;
  relation_kind?: unknown;
  resolution_status?: unknown;
  expand_ref?: unknown;
  evidence_refs?: unknown;
};
type SourceDataCandidate = Record<string, unknown> & {
  operation?: unknown;
  source_ref?: unknown;
  content?: unknown;
};

function asRecord<T extends Record<string, unknown>>(value: unknown): T | null {
  return isRecord(value) ? value as unknown as T : null;
}

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return Object.keys(value).length === keys.length
    && keys.every((key) => Object.prototype.hasOwnProperty.call(value, key));
}

function isID(value: unknown): value is string {
  return typeof value === "string" && value.length >= 1 && value.length <= 192;
}

function isUUID(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value);
}

function isHash(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
}

function boundedSignal(signal?: AbortSignal): AbortSignal {
  const timeout = AbortSignal.timeout(REQUEST_TIMEOUT_MS);
  return signal ? AbortSignal.any([signal, timeout]) : timeout;
}

function csrfToken(): string {
  const token = document.cookie
    .split(";")
    .map((part) => part.trim())
    .find((part) => part.startsWith("mce4d_csrf="))
    ?.slice("mce4d_csrf=".length);
  if (!token) {
    throw new ExplorerRequestError("Phiên Explorer thiếu CSRF token; hãy xác thực lại.", "CSRF_MISSING");
  }
  return decodeURIComponent(token);
}

function parseEnvelope<T>(value: unknown): ApiEnvelope<T> {
  const body = asRecord<EnvelopeCandidate>(value);
  if (!body || body.schema_version !== 1 || typeof body.request_id !== "string") {
    throw new ExplorerRequestError("Explorer trả về envelope không hợp lệ.", "INVALID_RESPONSE");
  }

  const status = body.status;
  if (status !== "ok" && status !== "partial" && status !== "blocked" && status !== "error") {
    throw new ExplorerRequestError("Explorer trả về trạng thái không hợp lệ.", "INVALID_RESPONSE");
  }
  if (status === "blocked" || status === "error") {
    const error = asRecord<ErrorCandidate>(body.error);
    const message = error && typeof error.message === "string"
      ? error.message
      : "Explorer không thể hoàn tất yêu cầu.";
    const code = error && typeof error.code === "string" ? error.code : null;
    throw new ExplorerRequestError(message, code);
  }
  if (body.data === null || body.data === undefined) {
    throw new ExplorerRequestError("Explorer không trả dữ liệu cho yêu cầu này.", "INVALID_RESPONSE");
  }
  return body as unknown as ApiEnvelope<T>;
}

async function request<T>(
  path: string,
  init: RequestInit,
): Promise<ApiEnvelope<T>> {
  assertLoopbackHost();
  let response: Response;
  try {
    response = await fetch(`${API_PREFIX}${path}`, {
      ...init,
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        ...init.headers,
      },
      signal: init.signal ?? AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") {
      throw error;
    }
    if (error instanceof DOMException && error.name === "TimeoutError") {
      throw new ExplorerRequestError("Yêu cầu tới Explorer đã quá thời hạn.", "REQUEST_TIMEOUT");
    }
    throw new ExplorerRequestError("Không thể kết nối tới Explorer trên loopback.", "NETWORK_ERROR");
  }

  let body: unknown;
  try {
    body = await response.json();
  } catch {
    throw new ExplorerRequestError("Explorer trả về JSON không hợp lệ.", "INVALID_RESPONSE");
  }

  if (!response.ok) {
    const bodyRecord = asRecord<EnvelopeCandidate>(body);
    const error = asRecord<ErrorCandidate>(bodyRecord?.error);
    if (error) {
      const message = typeof error.message === "string"
        ? error.message
        : "Explorer từ chối yêu cầu.";
      const code = typeof error.code === "string" ? error.code : null;
      throw new ExplorerRequestError(message, code);
    }
    throw new ExplorerRequestError("Explorer từ chối yêu cầu.", "REQUEST_REJECTED");
  }
  return parseEnvelope<T>(body);
}

function assertLoopbackHost(): void {
  if (window.location.hostname !== "127.0.0.1") {
    throw new ExplorerRequestError("Explorer chỉ hỗ trợ origin loopback 127.0.0.1.", "UNSUPPORTED_ORIGIN");
  }
}

export function createExplorerApi(): ExplorerApi {
  return {
    async bootstrap(code, signal) {
      assertLoopbackHost();
      const response = await request<BootstrapData>("/session/bootstrap", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ code }),
        signal: boundedSignal(signal),
      });
      if (response.data?.operation !== "bootstrap" || !response.data.bearer) {
        throw new ExplorerRequestError("Explorer không tạo được phiên hợp lệ.", "INVALID_RESPONSE");
      }
      return response;
    },

    workspace(bearer, signal) {
      return request<WorkspaceData>("/workspace", {
        method: "GET",
        headers: { Authorization: `Bearer ${bearer}` },
        signal: boundedSignal(signal),
      }).then((response) => {
        const data = response.data;
        if (data?.operation !== "status"
          || (data.index_state !== "missing" && data.index_state !== "published" && data.index_state !== "building_with_previous" && data.index_state !== "unavailable")) {
          throw new ExplorerRequestError("Explorer trả workspace status không hợp lệ.", "INVALID_RESPONSE");
        }
        return response;
      });
    },

    graph(bearer, signal) {
      return request<unknown>("/graph/query", {
        method: "POST",
        headers: {
          Authorization: `Bearer ${bearer}`,
          "Content-Type": "application/json",
          "X-MCE4D-CSRF": csrfToken(),
        },
        body: JSON.stringify({
          root: { kind: "workspace" },
          depth: GRAPH_DEPTH,
          node_limit: GRAPH_NODE_LIMIT,
          edge_limit: GRAPH_EDGE_LIMIT,
        }),
        signal: boundedSignal(signal),
      }).then((response) => {
        if (!isGraphData(response.data)) {
          throw new ExplorerRequestError("Explorer trả graph data không hợp lệ.", "INVALID_RESPONSE");
        }
        return response as ApiEnvelope<GraphData>;
      });
    },

    readSource(bearer, sourceRef, signal) {
      return request<unknown>("/source/read", {
        method: "POST",
        headers: {
          Authorization: `Bearer ${bearer}`,
          "Content-Type": "application/json",
          "X-MCE4D-CSRF": csrfToken(),
        },
        body: JSON.stringify({ source_ref: sourceRef, max_output_bytes: 65_536 }),
        signal: boundedSignal(signal),
      }).then((response) => {
        const data = asRecord<SourceDataCandidate>(response.data);
        if (!data
          || data.operation !== "source_read"
          || typeof data.content !== "string"
          || !isSourceRef(data.source_ref)
          || sourceRefKey(data.source_ref) !== sourceRefKey(sourceRef)) {
          throw new ExplorerRequestError("Source response không khớp với ref đã chọn.", "SOURCE_REF_MISMATCH");
        }
        return response as ApiEnvelope<SourceData>;
      });
    },
  };
}

export function isSourceRef(value: unknown): value is SourceRef {
  const ref = asRecord<SourceRefCandidate>(value);
  const range = asRecord<ByteRangeCandidate>(ref?.range);
  if (!ref || !hasExactKeys(ref, ["worktree_id", "profile_id", "generation_id", "path", "content_hash", "range"])
    || !range
    || !hasExactKeys(range, ["start_byte", "end_byte"])) {
    return false;
  }
  return isUUID(ref.worktree_id)
    && isHash(ref.profile_id)
    && isUUID(ref.generation_id)
    && typeof ref.path === "string"
    && ref.path.length >= 1
    && ref.path.length <= 4096
    && isHash(ref.content_hash)
    && typeof range.start_byte === "number"
    && Number.isSafeInteger(range.start_byte)
    && typeof range.end_byte === "number"
    && Number.isSafeInteger(range.end_byte)
    && range.start_byte >= 0
    && range.end_byte >= range.start_byte;
}

function isGraphRef(value: unknown): boolean {
  const ref = asRecord(value);
  const kind = ref?.["kind"];
  if (!ref || typeof kind !== "string") return false;

  const common = ["kind", "worktree_id", "profile_id", "generation_id"];
  if (!isUUID(ref["worktree_id"]) || !isHash(ref["profile_id"]) || !isUUID(ref["generation_id"])) return false;
  switch (kind) {
    case "symbol":
      return hasExactKeys(ref, [...common, "symbol_id"]) && isID(ref["symbol_id"]);
    case "entity":
      return hasExactKeys(ref, [...common, "entity_id"]) && isID(ref["entity_id"]);
    case "module":
      return hasExactKeys(ref, [...common, "module_id"]) && isID(ref["module_id"]);
    case "source_anchor":
      return hasExactKeys(ref, [...common, "build_map_id", "anchor_id", "binding_id"])
        && isHash(ref["build_map_id"])
        && isHash(ref["anchor_id"])
        && isHash(ref["binding_id"]);
    case "file":
      return hasExactKeys(ref, [...common, "path", "content_hash"])
        && typeof ref["path"] === "string"
        && ref["path"].length >= 1
        && ref["path"].length <= 4096
        && (ref["content_hash"] === null || isHash(ref["content_hash"]));
    default:
      return false;
  }
}

const evidenceKinds = new Set([
  "syntax_exact", "type_resolved", "type_possible", "explicit_document", "text_match",
  "unresolved", "build_anchored", "runtime_observed", "manual_declared", "proposed",
]);
const evidenceOrigins = new Set([
  "static_analyzer", "build_instrumentation", "runtime_observation", "manual_approved", "ai_proposal",
]);

function isEvidenceRef(value: unknown): boolean {
  const ref = asRecord(value);
  if (!ref
    || !hasExactKeys(ref, ["evidence_id", "origin", "kind", "source_refs", "record_ref", "artifact_id"])
    || !isID(ref["evidence_id"])
    || typeof ref["origin"] !== "string"
    || !evidenceOrigins.has(ref["origin"])
    || typeof ref["kind"] !== "string"
    || !evidenceKinds.has(ref["kind"])
    || !Array.isArray(ref["source_refs"])
    || ref["source_refs"].length > 16
    || !ref["source_refs"].every(isSourceRef)
    || !(ref["artifact_id"] === null || isHash(ref["artifact_id"]))) {
    return false;
  }

  if (ref["record_ref"] === null) return true;
  const record = asRecord(ref["record_ref"]);
  return record !== null
    && hasExactKeys(record, ["record_id", "version"])
    && isUUID(record["record_id"])
    && typeof record["version"] === "number"
    && Number.isSafeInteger(record["version"])
    && record["version"] >= 1;
}

export function isGraphData(value: unknown): value is GraphData {
  const data = asRecord<GraphDataCandidate>(value);
  if (!data
    || !hasExactKeys(data, ["operation", "nodes", "edges", "subgraph_digest"])
    || data.operation !== "graph_query"
    || !Array.isArray(data.nodes)
    || data.nodes.length > GRAPH_NODE_LIMIT
    || !Array.isArray(data.edges)
    || data.edges.length > GRAPH_EDGE_LIMIT) {
    return false;
  }
  if (!isHash(data.subgraph_digest)) {
    return false;
  }

  const nodeIds = new Set<string>();
  const edgeIds = new Set<string>();
  for (const rawNode of data.nodes) {
    const item = asRecord<GraphNodeCandidate>(rawNode);
    if (!item
      || !hasExactKeys(item, ["node_id", "ref", "kind", "label", "plane", "evidence_refs", "source_refs", "expandable_count"])
      || !isID(item.node_id)
      || nodeIds.has(item.node_id)
      || !isGraphRef(item.ref)
      || !isID(item.kind)
      || typeof item.label !== "string"
      || item.label.length > 512
      || !(typeof item.expandable_count === "number" || item.expandable_count === null)
      || (typeof item.expandable_count === "number" && (!Number.isSafeInteger(item.expandable_count) || item.expandable_count < 0))
      || (item.plane !== "intended" && item.plane !== "observed_static" && item.plane !== "observed_runtime" && item.plane !== "annotation")
      || !Array.isArray(item.source_refs)
      || item.source_refs.length > 16
      || !Array.isArray(item.evidence_refs)
      || item.evidence_refs.length > 16) {
      return false;
    }
    if (!item.source_refs.every(isSourceRef) || !item.evidence_refs.every(isEvidenceRef)) {
      return false;
    }
    nodeIds.add(item.node_id);
  }

  for (const rawEdge of data.edges) {
    const item = asRecord<GraphEdgeCandidate>(rawEdge);
    if (!item
      || !hasExactKeys(item, ["edge_id", "from_node_id", "to_node_id", "relation_kind", "resolution_status", "evidence_refs", "expand_ref"])
      || !isID(item.edge_id)
      || edgeIds.has(item.edge_id)
      || !isID(item.from_node_id)
      || !isID(item.to_node_id)
      || !isID(item.relation_kind)
      || (item.resolution_status !== "exact_in_supported_scope" && item.resolution_status !== "candidate" && item.resolution_status !== "unresolved")
      || !(item.expand_ref === null || (typeof item.expand_ref === "string" && item.expand_ref.length >= 1 && item.expand_ref.length <= 32_768))
      || !Array.isArray(item.evidence_refs)
      || item.evidence_refs.length > 16
      || !item.evidence_refs.every(isEvidenceRef)) {
      return false;
    }
    edgeIds.add(item.edge_id);
  }
  return true;
}
