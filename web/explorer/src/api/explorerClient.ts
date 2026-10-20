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
  if (!ref || !range) {
    return false;
  }
  return typeof ref.worktree_id === "string"
    && typeof ref.profile_id === "string"
    && typeof ref.generation_id === "string"
    && typeof ref.path === "string"
    && typeof ref.content_hash === "string"
    && /^[0-9a-f]{64}$/.test(ref.content_hash)
    && typeof range.start_byte === "number"
    && Number.isSafeInteger(range.start_byte)
    && typeof range.end_byte === "number"
    && Number.isSafeInteger(range.end_byte)
    && range.start_byte >= 0
    && range.end_byte >= range.start_byte;
}

export function isGraphData(value: unknown): value is GraphData {
  const data = asRecord<GraphDataCandidate>(value);
  if (!data || data.operation !== "graph_query" || !Array.isArray(data.nodes) || !Array.isArray(data.edges)) {
    return false;
  }
  if (typeof data.subgraph_digest !== "string" || !/^[0-9a-f]{64}$/.test(data.subgraph_digest)) {
    return false;
  }

  const nodeIds = new Set<string>();
  const edgeIds = new Set<string>();
  for (const rawNode of data.nodes) {
    const item = asRecord<GraphNodeCandidate>(rawNode);
    if (!item
      || typeof item.node_id !== "string"
      || nodeIds.has(item.node_id)
      || typeof item.kind !== "string"
      || typeof item.label !== "string"
      || !(typeof item.expandable_count === "number" || item.expandable_count === null)
      || (typeof item.expandable_count === "number" && (!Number.isSafeInteger(item.expandable_count) || item.expandable_count < 0))
      || (item.plane !== "intended" && item.plane !== "observed_static" && item.plane !== "observed_runtime" && item.plane !== "annotation")
      || !Array.isArray(item.source_refs)
      || !Array.isArray(item.evidence_refs)) {
      return false;
    }
    if (!item.source_refs.every(isSourceRef)) {
      return false;
    }
    nodeIds.add(item.node_id);
  }

  for (const rawEdge of data.edges) {
    const item = asRecord<GraphEdgeCandidate>(rawEdge);
    if (!item
      || typeof item.edge_id !== "string"
      || edgeIds.has(item.edge_id)
      || typeof item.from_node_id !== "string"
      || typeof item.to_node_id !== "string"
      || typeof item.relation_kind !== "string"
      || (item.resolution_status !== "exact_in_supported_scope" && item.resolution_status !== "candidate" && item.resolution_status !== "unresolved")
      || !(typeof item.expand_ref === "string" || item.expand_ref === null)
      || !Array.isArray(item.evidence_refs)) {
      return false;
    }
    edgeIds.add(item.edge_id);
  }
  return true;
}
