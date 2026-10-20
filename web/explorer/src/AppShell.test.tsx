import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppShell } from "./AppShell";
import type {
  ApiEnvelope,
  BootstrapData,
  ExplorerApi,
  GraphData,
  SourceData,
  SourceRef,
  WorkspaceData,
} from "./api/types";

const sourceRef: SourceRef = {
  worktree_id: "88ce9543-7114-4263-ab0e-7ce99632b17a",
  profile_id: "a".repeat(64),
  generation_id: "60ce9543-7114-4263-ab0e-7ce99632b17a",
  path: "src/orders.ts",
  content_hash: "b".repeat(64),
  range: { start_byte: 0, end_byte: 24 },
};

// UI-only synthetic graph data; production code never imports this as a fallback or integration oracle.
const syntheticTestGraph: GraphData = {
  operation: "graph_query",
  subgraph_digest: "c".repeat(64),
  nodes: [
    {
      node_id: "module:orders",
      kind: "module",
      label: "Orders module",
      plane: "observed_static",
      ref: { kind: "module" },
      evidence_refs: [],
      source_refs: [sourceRef],
      expandable_count: 0,
    },
    {
      node_id: "symbol:save-order",
      kind: "function",
      label: "saveOrder",
      plane: "observed_static",
      ref: { kind: "symbol" },
      evidence_refs: [],
      source_refs: [],
      expandable_count: null,
    },
  ],
  edges: [
    {
      edge_id: "relation:contains",
      from_node_id: "module:orders",
      to_node_id: "symbol:save-order",
      relation_kind: "contains",
      resolution_status: "exact_in_supported_scope",
      evidence_refs: [],
      expand_ref: null,
    },
    {
      edge_id: "relation:outside-subgraph",
      from_node_id: "module:orders",
      to_node_id: "module:not-returned",
      relation_kind: "depends_on",
      resolution_status: "candidate",
      evidence_refs: [],
      expand_ref: null,
    },
  ],
};

function envelope<T>(data: T, status: "ok" | "partial" = "ok"): ApiEnvelope<T> {
  return {
    schema_version: 1,
    request_id: "60ce9543-7114-4263-ab0e-7ce99632b17a",
    status,
    scope: null,
    freshness: {
      status: "verified",
      source_view: "indexed_snapshot",
      scope: "workspace",
      buffer_scope: "disk_only",
      consistency: "captured_bytes_with_observed_stability",
    },
    readiness: "reviewable",
    data,
    pagination: {
      truncated: false,
      enumeration_complete: true,
      returned_count: 2,
      required_pending: null,
      optional_pending: null,
      next_cursor: null,
      limit_reason: null,
    },
    warnings: [],
    error: null,
  };
}

function workspace(index_state: WorkspaceData["index_state"]): WorkspaceData {
  return {
    operation: "status",
    index_state,
    project_stage: "implemented_scope",
    scopes: [],
    capabilities: [],
    registered_scopes: [],
  };
}

function createApi(indexState: WorkspaceData["index_state"], graphData: GraphData | null = syntheticTestGraph) {
  const api: ExplorerApi = {
    bootstrap: vi.fn(async () => envelope<BootstrapData>({
      operation: "bootstrap",
      bearer: "in-memory-test-bearer",
      expires_at: "2026-10-20T06:00:00Z",
      capabilities: ["workspace.read", "graph.read", "source.read"],
    })),
    workspace: vi.fn(async () => envelope(workspace(indexState))),
    graph: vi.fn(async () => {
      if (!graphData) throw new Error("graph endpoint must not be called for an unindexed workspace");
      return envelope(graphData, "partial");
    }),
    readSource: vi.fn(async () => envelope<SourceData>({
      operation: "source_read",
      source_ref: sourceRef,
      content: "export function saveOrder() {}\n",
    })),
  };
  return api;
}

async function connect(api: ExplorerApi): Promise<void> {
  render(<AppShell api={api} />);
  fireEvent.change(screen.getByLabelText("Mã bootstrap"), { target: { value: "one-time-code" } });
  fireEvent.click(screen.getByRole("button", { name: "Kết nối an toàn" }));
}

describe("Explorer AppShell", () => {
  beforeEach(() => vi.clearAllMocks());

  it("does not request or synthesize a graph when the workspace has no index", async () => {
    const api = createApi("missing", null);
    await connect(api);

    expect(await screen.findByText("Workspace chưa có index")).toBeTruthy();
    expect(screen.getByText("Không có graph để hiển thị. Explorer không thay bằng dữ liệu demo.")).toBeTruthy();
    expect(api.graph).not.toHaveBeenCalled();
    expect(screen.queryByText("Orders module")).toBeNull();
  });

  it("keeps graph selection, accessible node navigation, source read, and splitter state connected", async () => {
    const api = createApi("published");
    await connect(api);

    const listbox = await screen.findByRole("listbox", { name: "Các nút trong đồ thị" });
    const options = within(listbox).getAllByRole("option");
    const moduleOption = options.find((option) => option.textContent?.includes("Orders module"));
    expect(moduleOption).toBeDefined();
    if (!moduleOption) throw new Error("indexed test fixture node missing from semantic list");
    expect(screen.getByText("API đánh dấu dữ liệu partial.")).toBeTruthy();
    expect(screen.getByText(/1 quan hệ có đầu mút ngoài subgraph hiện tại/)).toBeTruthy();
    fireEvent.keyDown(moduleOption, { key: "ArrowDown" });
    await waitFor(() => expect(within(listbox).getAllByRole("option")[1]?.getAttribute("aria-selected")).toBe("true"));
    expect(within(listbox).getAllByRole("option")[0]?.getAttribute("tabindex")).toBe("-1");
    expect(within(listbox).getAllByRole("option")[1]?.getAttribute("tabindex")).toBe("0");

    const firstOption = within(listbox).getAllByRole("option")[0];
    expect(firstOption).toBeDefined();
    if (!firstOption) throw new Error("first semantic graph node missing");
    fireEvent.click(firstOption);
    expect(screen.getByText("Node ID").parentElement?.textContent).toContain("module:orders");
    fireEvent.click(screen.getByRole("button", { name: /src\/orders\.ts/ }));
    expect(await screen.findByText("export function saveOrder() {}", { selector: "code" })).toBeTruthy();
    expect(api.readSource).toHaveBeenCalledWith("in-memory-test-bearer", sourceRef, expect.any(AbortSignal));

    const sourceTab = screen.getByRole("tab", { name: "Source" });
    fireEvent.keyDown(sourceTab, { key: "ArrowLeft" });
    expect(screen.getByRole("tab", { name: "Chi tiết" }).getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(screen.getByRole("tab", { name: "Chi tiết" }), { key: "ArrowRight" });
    expect(screen.getByRole("tab", { name: "Source" }).getAttribute("aria-selected")).toBe("true");
    expect(await screen.findByText("export function saveOrder() {}", { selector: "code" })).toBeTruthy();

    const splitter = screen.getByRole("separator", { name: "Điều chỉnh độ rộng hai pane" });
    expect(splitter.getAttribute("aria-valuenow")).toBe("45");
    fireEvent.keyDown(splitter, { key: "ArrowRight" });
    expect(splitter.getAttribute("aria-valuenow")).toBe("50");
    expect(within(listbox).getAllByRole("option")[0]?.getAttribute("aria-selected")).toBe("true");
  });

  it("shows an empty indexed response without adding graph nodes", async () => {
    const emptyGraph: GraphData = {
      operation: "graph_query",
      nodes: [],
      edges: [],
      subgraph_digest: "d".repeat(64),
    };
    const api = createApi("published", emptyGraph);
    await connect(api);

    expect(await screen.findByText("Truy vấn không có nút")).toBeTruthy();
    expect(api.graph).toHaveBeenCalledOnce();
    expect(screen.queryByRole("option")).toBeNull();
  });

  it("keeps workspace provenance visible when the graph request fails", async () => {
    const api = createApi("published");
    api.graph = vi.fn(async () => { throw new Error("Dịch vụ graph tạm thời không khả dụng."); });
    await connect(api);

    expect(await screen.findByText("Dịch vụ graph tạm thời không khả dụng.")).toBeTruthy();
    expect(screen.getByText("Đang dùng snapshot đã xuất bản")).toBeTruthy();
    expect(screen.queryByRole("option")).toBeNull();
    expect(screen.queryByText("Orders module")).toBeNull();
  });

  it("shows the prior snapshot as updating without losing its indexed provenance", async () => {
    const api = createApi("building_with_previous");
    await connect(api);

    expect(await screen.findByText("Index đang cập nhật")).toBeTruthy();
    expect(screen.getByText("API cho biết snapshot trước vẫn còn; dữ liệu được đánh dấu đang cập nhật.")).toBeTruthy();
    expect(within(await screen.findByRole("listbox", { name: "Các nút trong đồ thị" })).getAllByRole("option")).toHaveLength(2);
  });
});
