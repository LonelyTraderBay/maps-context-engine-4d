import { useCallback, useEffect, useMemo, useReducer, useState } from "react";
import type { CSSProperties, FormEvent, KeyboardEvent } from "react";

import { createExplorerApi } from "./api/explorerClient";
import type { ApiEnvelope, ExplorerApi, GraphData, SourceRef, WorkspaceData } from "./api/types";
import { ExplorerRequestError } from "./api/types";
import { GraphPane } from "./features/graph/GraphPane";
import { InspectorPane } from "./features/source/InspectorPane";
import { initialSelection, selectionReducer, sourceRefKey } from "./state/selection";

interface AppShellProps {
  api?: ExplorerApi;
}

const defaultExplorerApi = createExplorerApi();

type WorkspaceSnapshot = {
  data: WorkspaceData;
  status: "ok" | "partial";
  freshness: ApiEnvelope<WorkspaceData>["freshness"];
};

type GraphSnapshot = {
  data: GraphData;
  status: "ok" | "partial";
  freshness: ApiEnvelope<GraphData>["freshness"];
  truncated: boolean;
};

function isIndexed(state: WorkspaceData["index_state"]): boolean {
  return state === "published" || state === "building_with_previous";
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "Có lỗi khi tải dữ liệu Explorer.";
}

function WorkspaceState({ state }: { state: WorkspaceData["index_state"] }) {
  const copy: Record<WorkspaceData["index_state"], { title: string; body: string }> = {
    missing: { title: "Workspace chưa có index", body: "Không có graph để hiển thị. Explorer không thay bằng dữ liệu demo." },
    published: { title: "Đang dùng snapshot đã xuất bản", body: "Graph và source được đọc qua API từ snapshot này." },
    building_with_previous: { title: "Index đang cập nhật", body: "API cho biết snapshot trước vẫn còn; dữ liệu được đánh dấu đang cập nhật." },
    unavailable: { title: "Index không khả dụng", body: "Không thể xác minh graph hiện tại. Source và graph không được dựng từ dữ liệu thay thế." },
  };
  return <div className={`workspace-state state-${state}`} role="status"><strong>{copy[state].title}</strong><span>{copy[state].body}</span></div>;
}

function AppHeader({ onReload, loading, expiresAt }: { onReload(): void; loading: boolean; expiresAt: string }) {
  return (
    <header className="app-header">
      <a className="brand" href="#app" aria-label="Maps Context Engine Explorer">
        <span className="brand-mark" aria-hidden="true">M4</span>
        <span><strong>Maps Context Engine</strong><small>Explorer · snapshot view</small></span>
      </a>
      <div className="header-actions">
        <span className="session-state"><span aria-hidden="true" className="status-dot" />Loopback session</span>
        <span className="session-expiry">Hết hạn {new Date(expiresAt).toLocaleTimeString()}</span>
        <button className="quiet-button" onClick={onReload} disabled={loading}>
          {loading ? "Đang tải…" : "Tải lại snapshot"}
        </button>
      </div>
    </header>
  );
}

export function AppShell({ api = defaultExplorerApi }: AppShellProps) {
  const [bootstrapCode, setBootstrapCode] = useState("");
  const [bearer, setBearer] = useState<string | null>(null);
  const [expiresAt, setExpiresAt] = useState<string | null>(null);
  const [workspace, setWorkspace] = useState<WorkspaceSnapshot | null>(null);
  const [graph, setGraph] = useState<GraphSnapshot | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [split, setSplit] = useState(45);
  const [narrowLayout, setNarrowLayout] = useState(() => window.matchMedia("(max-width: 620px)").matches);
  const [selection, dispatch] = useReducer(selectionReducer, initialSelection);
  const [reloadToken, setReloadToken] = useState(0);

  const reload = useCallback(() => setReloadToken((value) => value + 1), []);

  useEffect(() => {
    const media = window.matchMedia("(max-width: 620px)");
    const update = () => setNarrowLayout(media.matches);
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);

  useEffect(() => {
    if (!bearer) return;
    const activeBearer = bearer;
    const controller = new AbortController();
    let current = true;
    setLoading(true);
    setError(null);
    setWorkspace(null);
    setGraph(null);

    async function load(): Promise<void> {
      try {
        const workspaceResponse = await api.workspace(activeBearer, controller.signal);
        if (!current || !workspaceResponse.data) return;
        const workspaceSnapshot: WorkspaceSnapshot = {
          data: workspaceResponse.data,
          status: workspaceResponse.status === "partial" ? "partial" : "ok",
          freshness: workspaceResponse.freshness,
        };
        setWorkspace(workspaceSnapshot);
        if (!isIndexed(workspaceResponse.data.index_state)) {
          dispatch({ type: "clear" });
          return;
        }

        const graphResponse = await api.graph(activeBearer, controller.signal);
        if (!current || !graphResponse.data) return;
        setGraph({
          data: graphResponse.data,
          status: graphResponse.status === "partial" ? "partial" : "ok",
          freshness: graphResponse.freshness,
          truncated: graphResponse.pagination.truncated,
        });
      } catch (cause) {
        if (!current || (cause instanceof DOMException && cause.name === "AbortError")) return;
        if (cause instanceof ExplorerRequestError && ["AUTH_REQUIRED", "AUTH_INVALID", "AUTH_EXPIRED"].includes(cause.code ?? "")) {
          setBearer(null);
          setExpiresAt(null);
          setWorkspace(null);
          setGraph(null);
        }
        setError(errorMessage(cause));
      } finally {
        if (current) setLoading(false);
      }
    }

    void load();
    return () => {
      current = false;
      controller.abort();
    };
  }, [api, bearer, reloadToken]);

  const selectedNode = useMemo(
    () => graph?.data.nodes.find((node) => node.node_id === selection.selectedNodeId) ?? null,
    [graph, selection.selectedNodeId],
  );
  const activeSourceRef = useMemo<SourceRef | null>(() => {
    if (!selectedNode || !selection.selectedSourceKey) return null;
    return selectedNode.source_refs.find((sourceRef) => sourceRefKey(sourceRef) === selection.selectedSourceKey) ?? null;
  }, [selectedNode, selection.selectedSourceKey]);

  useEffect(() => {
    if (selection.selectedNodeId && !selectedNode) {
      dispatch({ type: "clear" });
    }
  }, [selectedNode, selection.selectedNodeId]);

  async function submitBootstrap(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    const code = bootstrapCode.trim();
    if (!code || loading) return;
    setLoading(true);
    setError(null);
    try {
      const response = await api.bootstrap(code);
      if (!response.data) throw new ExplorerRequestError("Bootstrap response thiếu session.", "INVALID_RESPONSE");
      setBearer(response.data.bearer);
      setExpiresAt(response.data.expires_at);
      setBootstrapCode("");
    } catch (cause) {
      setError(errorMessage(cause));
      setLoading(false);
    }
  }

  function resizeByKeyboard(event: KeyboardEvent<HTMLDivElement>): void {
    const increments: Record<string, number> = narrowLayout
      ? { ArrowUp: -5, ArrowDown: 5 }
      : { ArrowLeft: -5, ArrowRight: 5 };
    if (event.key in increments) {
      event.preventDefault();
      setSplit((value) => Math.max(30, Math.min(70, value + (increments[event.key] ?? 0))));
    } else if (event.key === "Home") {
      event.preventDefault();
      setSplit(30);
    } else if (event.key === "End") {
      event.preventDefault();
      setSplit(70);
    }
  }

  function selectSource(sourceRef: SourceRef): void {
    dispatch({ type: "select-source", sourceKey: sourceRefKey(sourceRef) });
  }

  if (!bearer || !expiresAt) {
    return (
      <main id="app" className="auth-shell">
      <a className="skip-link" href="#bootstrap-code">Bỏ qua phần đầu trang</a>
        <div className="auth-card">
          <a className="brand auth-brand" href="#app">
            <span className="brand-mark" aria-hidden="true">M4</span>
            <span><strong>Maps Context Engine</strong><small>Local Explorer</small></span>
          </a>
          <p className="eyebrow">Kết nối loopback</p>
          <h1>Mở workspace đã lập chỉ mục</h1>
          <p className="auth-intro">Nhập mã bootstrap một lần do CLI trên máy này cung cấp. Mã và bearer chỉ được giữ trong bộ nhớ của tab.</p>
          <form onSubmit={(event) => void submitBootstrap(event)}>
            <label htmlFor="bootstrap-code">Mã bootstrap</label>
            <input
              id="bootstrap-code"
              type="password"
              autoComplete="off"
              spellCheck={false}
              value={bootstrapCode}
              onChange={(event) => setBootstrapCode(event.target.value)}
              placeholder="Dán mã từ CLI"
              disabled={loading}
            />
            {error && <p className="inline-error" role="alert">{error}</p>}
            <button className="primary-button" type="submit" disabled={loading || bootstrapCode.trim().length === 0}>
              {loading ? "Đang xác thực…" : "Kết nối an toàn"}
            </button>
          </form>
          <p className="security-note"><span aria-hidden="true">●</span> API chỉ dùng origin 127.0.0.1; không lưu token vào URL hay localStorage.</p>
        </div>
      </main>
    );
  }

  const state = workspace?.data.index_state;
  const snapshotNote = workspace
    ? `${workspace.freshness.status} · ${workspace.freshness.source_view}`
    : "Đang xác minh snapshot";
  const graphError = error;

  return (
    <main id="app" className="explorer-app" style={{ "--split": `${split}%` } as CSSProperties}>
      <a className="skip-link" href="#graph-title">Đi tới đồ thị</a>
      <AppHeader onReload={reload} loading={loading} expiresAt={expiresAt} />
      <div className="workspace-toolbar">
        <div className="workspace-breadcrumb"><span>Workspace</span><span aria-hidden="true">/</span><strong>Indexed source</strong></div>
        {workspace && <WorkspaceState state={workspace.data.index_state} />}
        <span className="freshness-label">{snapshotNote}</span>
      </div>

      <div className="split-layout">
        <GraphPane
          data={graph?.data ?? null}
          selectedNodeId={selection.selectedNodeId}
          busy={loading && graph === null && (workspace === null || isIndexed(workspace.data.index_state))}
          error={graphError}
          onSelectNode={(nodeId) => dispatch({ type: "select-node", nodeId })}
        />
        <div
          className="splitter"
          role="separator"
          aria-label="Điều chỉnh độ rộng hai pane"
          aria-orientation={narrowLayout ? "horizontal" : "vertical"}
          aria-valuemin={30}
          aria-valuemax={70}
          aria-valuenow={split}
          aria-valuetext={`Pane đồ thị ${split} phần trăm`}
          aria-controls="graph-pane inspector-pane"
          tabIndex={0}
          onKeyDown={resizeByKeyboard}
          onPointerDown={(event) => {
            event.currentTarget.setPointerCapture?.(event.pointerId);
            event.currentTarget.dataset["dragStart"] = `${narrowLayout ? event.clientY : event.clientX}:${split}`;
          }}
          onPointerMove={(event) => {
            const start = event.currentTarget.dataset["dragStart"];
            if (!start) return;
            const [startXRaw, startSplitRaw] = start.split(":");
            const startX = Number(startXRaw);
            const startSplit = Number(startSplitRaw);
            const bounds = event.currentTarget.parentElement?.getBoundingClientRect();
            const extent = narrowLayout ? bounds?.height ?? 0 : bounds?.width ?? 0;
            const pointer = narrowLayout ? event.clientY : event.clientX;
            if (extent > 0 && Number.isFinite(startX) && Number.isFinite(startSplit)) {
              setSplit(Math.max(30, Math.min(70, startSplit + ((pointer - startX) / extent) * 100)));
            }
          }}
          onPointerUp={(event) => { delete event.currentTarget.dataset["dragStart"]; }}
          onPointerCancel={(event) => { delete event.currentTarget.dataset["dragStart"]; }}
        />
        <InspectorPane
          node={selectedNode}
          activeTab={selection.inspectorTab}
          activeSourceRef={activeSourceRef}
          bearer={bearer}
          api={api}
          onSelectTab={(tab) => dispatch({ type: "select-tab", tab })}
          onSelectSource={selectSource}
        />
      </div>
      <footer className="app-footer">
        <span><span aria-hidden="true" className="status-dot" />{state ? `Index: ${state}` : "Đang tải index state"}</span>
        {graph && <span>Subgraph SHA-256 · <code>{graph.data.subgraph_digest.slice(0, 16)}</code></span>}
        {graph?.truncated && <span role="status">Đồ thị bị giới hạn theo budget API.</span>}
        {graph?.status === "partial" && <span role="status">API đánh dấu dữ liệu partial.</span>}
      </footer>
    </main>
  );
}
