import { useEffect, useState } from "react";
import type { KeyboardEvent } from "react";

import type { ApiEnvelope, ExplorerApi, GraphNode, SourceData, SourceRef } from "../../api/types";
import { ExplorerRequestError } from "../../api/types";
import { sourceRefKey } from "../../state/selection";

interface InspectorPaneProps {
  node: GraphNode | null;
  activeTab: "details" | "source";
  activeSourceRef: SourceRef | null;
  bearer: string;
  api: ExplorerApi;
  onSelectTab(tab: "details" | "source"): void;
  onSelectSource(sourceRef: SourceRef): void;
}

function SourceContent({ bearer, api, sourceRef }: { bearer: string; api: ExplorerApi; sourceRef: SourceRef }) {
  const [result, setResult] = useState<{ content: string; data: SourceData } | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    let current = true;
    setLoading(true);
    setError(null);
    setResult(null);
    api.readSource(bearer, sourceRef, controller.signal)
      .then((response: ApiEnvelope<SourceData>) => {
        if (current && response.data) {
          setResult({ content: response.data.content, data: response.data });
        }
      })
      .catch((cause: unknown) => {
        if (!current || (cause instanceof DOMException && cause.name === "AbortError")) return;
        setError(cause instanceof ExplorerRequestError ? cause.message : "Không đọc được source từ snapshot.");
      })
      .finally(() => {
        if (current) setLoading(false);
      });

    return () => {
      current = false;
      controller.abort();
    };
  }, [api, bearer, sourceRef]);

  if (loading) return <p className="inline-status" role="status">Đang đọc source đã ghim trong snapshot…</p>;
  if (error) return <p className="inline-error" role="alert">{error}</p>;
  if (!result) return <p className="muted">Không có source response.</p>;

  return (
    <div className="source-view">
      <dl className="source-meta">
        <div><dt>Đường dẫn</dt><dd>{result.data.source_ref.path}</dd></div>
        <div><dt>Hash nội dung</dt><dd className="mono">{result.data.source_ref.content_hash}</dd></div>
        <div><dt>Khoảng byte</dt><dd>{result.data.source_ref.range.start_byte}–{result.data.source_ref.range.end_byte}</dd></div>
      </dl>
      <pre><code>{result.content}</code></pre>
      <p className="source-note">Nội dung do API đọc từ indexed snapshot; đường dẫn không được mở trực tiếp bởi trình duyệt.</p>
    </div>
  );
}

export function InspectorPane({ node, activeTab, activeSourceRef, bearer, api, onSelectTab, onSelectSource }: InspectorPaneProps) {
  function handleTabKeyDown(event: KeyboardEvent<HTMLButtonElement>, current: "details" | "source"): void {
    const tabs: Array<"details" | "source"> = activeSourceRef ? ["details", "source"] : ["details"];
    const currentIndex = tabs.indexOf(current);
    const nextIndex = event.key === "Home"
      ? 0
      : event.key === "End"
        ? tabs.length - 1
        : event.key === "ArrowRight"
          ? (currentIndex + 1) % tabs.length
          : event.key === "ArrowLeft"
            ? (currentIndex - 1 + tabs.length) % tabs.length
            : -1;
    if (nextIndex < 0) return;
    event.preventDefault();
    const next = tabs[nextIndex];
    if (!next) return;
    onSelectTab(next);
    document.getElementById(`inspector-tab-${next}`)?.focus();
  }

  return (
    <section id="inspector-pane" className="inspector-pane" aria-labelledby="inspector-title">
      <header className="pane-heading">
        <div>
          <p className="eyebrow">Evidence theo selection</p>
          <h2 id="inspector-title">Source / chi tiết</h2>
        </div>
      </header>

      <div className="inspector-tabs" role="tablist" aria-label="Nội dung inspector">
        <button
          id="inspector-tab-details"
          role="tab"
          aria-selected={activeTab === "details"}
          aria-controls="inspector-panel"
          tabIndex={activeTab === "details" ? 0 : -1}
          onClick={() => onSelectTab("details")}
          onKeyDown={(event) => handleTabKeyDown(event, "details")}
        >Chi tiết</button>
        <button
          id="inspector-tab-source"
          role="tab"
          aria-selected={activeTab === "source"}
          aria-controls="inspector-panel"
          tabIndex={activeTab === "source" ? 0 : -1}
          onClick={() => onSelectTab("source")}
          onKeyDown={(event) => handleTabKeyDown(event, "source")}
          disabled={!activeSourceRef}
        >Source</button>
      </div>

      <div id="inspector-panel" role="tabpanel" aria-labelledby={`inspector-tab-${activeTab}`} tabIndex={0}>
        {!node && (
          <div className="empty-state inspector-empty" role="status">
            <span className="empty-icon" aria-hidden="true">↖</span>
            <h3>Chọn một nút ở pane trái</h3>
            <p>Pane này chỉ hiển thị source ref và evidence của selection hiện tại.</p>
          </div>
        )}

        {node && activeTab === "details" && (
          <div className="detail-content">
            <span className="node-kind">{node.kind}</span>
            <h3>{node.label || node.kind}</h3>
            <dl className="detail-meta">
              <div><dt>Node ID</dt><dd className="mono">{node.node_id}</dd></div>
              <div><dt>Plane</dt><dd>{node.plane}</dd></div>
              <div><dt>Evidence refs</dt><dd>{node.evidence_refs.length}</dd></div>
              {node.expandable_count !== null && <div><dt>Có thể mở rộng</dt><dd>{node.expandable_count}</dd></div>}
            </dl>
            <h4>Source refs ({node.source_refs.length})</h4>
            {node.source_refs.length === 0
              ? <p className="muted">Node này chưa có source ref trong phản hồi index.</p>
              : <ul className="source-ref-list">
                {node.source_refs.map((sourceRef) => (
                  <li key={sourceRefKey(sourceRef)}>
                    <button className="source-ref-button" onClick={() => onSelectSource(sourceRef)}>
                      <span>{sourceRef.path}</span>
                      <small>byte {sourceRef.range.start_byte}–{sourceRef.range.end_byte} · {sourceRef.content_hash.slice(0, 12)}</small>
                    </button>
                  </li>
                ))}
              </ul>}
          </div>
        )}

        {node && activeTab === "source" && activeSourceRef && (
          <SourceContent key={sourceRefKey(activeSourceRef)} bearer={bearer} api={api} sourceRef={activeSourceRef} />
        )}
      </div>
    </section>
  );
}
