import { beforeEach, describe, expect, it, vi } from "vitest";

import { createExplorerApi, isGraphData } from "./explorerClient";
import type { ApiEnvelope, BootstrapData, GraphData } from "./types";

function response<T>(data: T): Response {
  const envelope: ApiEnvelope<T> = {
    schema_version: 1,
    request_id: "00000000-0000-4000-8000-000000000001",
    status: "ok",
    scope: null,
    freshness: { status: "verified", source_view: "indexed_snapshot", scope: "workspace", buffer_scope: "disk_only", consistency: "captured_bytes_with_observed_stability" },
    readiness: "reviewable",
    data,
    pagination: { truncated: false, enumeration_complete: true, returned_count: 0, required_pending: null, optional_pending: null, next_cursor: null, limit_reason: null },
    warnings: [],
    error: null,
  };
  return new Response(JSON.stringify(envelope), { status: 200, headers: { "Content-Type": "application/json" } });
}

describe("Explorer API boundary", () => {
  beforeEach(() => {
    document.cookie = "mce4d_csrf=csrf-test-token; path=/";
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("exchanges the one-time code without placing credentials in the URL or headers", async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => response<BootstrapData>({
      operation: "bootstrap",
      bearer: "opaque-bearer",
      expires_at: "2026-10-20T06:00:00Z",
      capabilities: ["workspace.read"],
    }));
    vi.stubGlobal("fetch", fetchMock);

    await createExplorerApi().bootstrap("private-one-time-code");

    const call = fetchMock.mock.calls[0];
    expect(call).toBeDefined();
    const [url, init] = call ?? ["", undefined];
    expect(url).toBe("/api/explorer/v1/session/bootstrap");
    expect(JSON.stringify(url)).not.toContain("private-one-time-code");
    expect(init?.credentials).toBe("same-origin");
    expect(init?.headers).not.toHaveProperty("Authorization");
    expect(init?.body).toBe(JSON.stringify({ code: "private-one-time-code" }));
  });

  it("sends the in-memory bearer and CSRF token on bounded graph requests", async () => {
    const graph: GraphData = { operation: "graph_query", nodes: [], edges: [], subgraph_digest: "a".repeat(64) };
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => response(graph));
    vi.stubGlobal("fetch", fetchMock);

    await createExplorerApi().graph("opaque-bearer");

    const call = fetchMock.mock.calls[0];
    expect(call).toBeDefined();
    const [url, init] = call ?? ["", undefined];
    const request = init ?? {};
    const headers = new Headers(request.headers);
    expect(url).toBe("/api/explorer/v1/graph/query");
    expect(request.credentials).toBe("same-origin");
    expect(headers.get("Authorization")).toBe("Bearer opaque-bearer");
    expect(headers.get("X-MCE4D-CSRF")).toBe("csrf-test-token");
    expect(JSON.parse(String(request.body))).toEqual({ root: { kind: "workspace" }, depth: 3, node_limit: 200, edge_limit: 500 });
  });

  it("refuses credentialed API requests outside the loopback origin", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal("window", { location: { hostname: "localhost" } });

    await expect(createExplorerApi().workspace("opaque-bearer")).rejects.toThrow(
      "Explorer chỉ hỗ trợ origin loopback 127.0.0.1.",
    );
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects duplicate graph node identities instead of rendering ambiguous selection", () => {
    const node = {
      node_id: "duplicate",
      kind: "module",
      label: "Module",
      plane: "observed_static",
      ref: {},
      evidence_refs: [],
      source_refs: [],
      expandable_count: null,
    };
    expect(isGraphData({ operation: "graph_query", subgraph_digest: "a".repeat(64), nodes: [node, node], edges: [] })).toBe(false);
  });

  it("does not display source content when the API answers for a different snapshot ref", async () => {
    const mismatched = {
      operation: "source_read",
      source_ref: {
        worktree_id: "88ce9543-7114-4263-ab0e-7ce99632b17a",
        profile_id: "a".repeat(64),
        generation_id: "60ce9543-7114-4263-ab0e-7ce99632b17a",
        path: "src/other.ts",
        content_hash: "b".repeat(64),
        range: { start_byte: 0, end_byte: 24 },
      },
      content: "stale bytes",
    };
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => response(mismatched));
    vi.stubGlobal("fetch", fetchMock);

    await expect(createExplorerApi().readSource("opaque-bearer", {
      worktree_id: "88ce9543-7114-4263-ab0e-7ce99632b17a",
      profile_id: "a".repeat(64),
      generation_id: "60ce9543-7114-4263-ab0e-7ce99632b17a",
      path: "src/orders.ts",
      content_hash: "b".repeat(64),
      range: { start_byte: 0, end_byte: 24 },
    })).rejects.toThrow("Source response không khớp với ref đã chọn.");
  });
});
