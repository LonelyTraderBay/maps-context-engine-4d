import { useMemo } from "react";
import { Background, Controls, ReactFlow } from "@xyflow/react";

import type { GraphData, GraphNode } from "../../api/types";
import { layoutGraph } from "./layout";

interface GraphPaneProps {
  data: GraphData | null;
  selectedNodeId: string | null;
  busy: boolean;
  error: string | null;
  onSelectNode(nodeId: string): void;
}

function planeLabel(plane: GraphNode["plane"]): string {
  switch (plane) {
    case "intended": return "Dự kiến";
    case "observed_static": return "Tĩnh";
    case "observed_runtime": return "Runtime";
    case "annotation": return "Ghi chú";
  }
}

function optionId(nodeId: string): string {
  return `graph-option-${encodeURIComponent(nodeId)}`;
}

export function GraphPane({ data, selectedNodeId, busy, error, onSelectNode }: GraphPaneProps) {
  const graphNodes = useMemo(
    () => [...(data?.nodes ?? [])].sort((left, right) => left.node_id < right.node_id ? -1 : left.node_id > right.node_id ? 1 : 0),
    [data],
  );
  const layout = useMemo(
    () => data ? layoutGraph(data, selectedNodeId) : { nodes: [], edges: [] },
    [data, selectedNodeId],
  );
  const nodeById = useMemo(() => new Map(data?.nodes.map((node) => [node.node_id, node]) ?? []), [data]);
  const omittedEdgeCount = data?.edges.filter(
    (edge) => !nodeById.has(edge.from_node_id) || !nodeById.has(edge.to_node_id),
  ).length ?? 0;

  function moveSelection(currentId: string, direction: -1 | 1 | "first" | "last"): void {
    const nodes = graphNodes;
    if (nodes.length === 0) return;
    const currentIndex = nodes.findIndex((node) => node.node_id === currentId);
    const nextIndex = direction === "first"
      ? 0
      : direction === "last"
        ? nodes.length - 1
        : Math.max(0, Math.min(nodes.length - 1, currentIndex + direction));
    const next = nodes[nextIndex];
    if (next) {
      onSelectNode(next.node_id);
      document.getElementById(optionId(next.node_id))?.focus();
    }
  }

  return (
    <section id="graph-pane" className="graph-pane" aria-labelledby="graph-title">
      <header className="pane-heading">
        <div>
          <p className="eyebrow">Chế độ xem chỉ đọc</p>
          <h2 id="graph-title">Đồ thị đã lập chỉ mục</h2>
        </div>
        {data && <span className="count-badge">{data.nodes.length} nút · {data.edges.length} quan hệ</span>}
      </header>

      {error && <p className="inline-error" role="alert">{error}</p>}
      {busy && <p className="inline-status" role="status">Đang tải trạng thái workspace…</p>}
      {!busy && !error && !data && (
        <div className="empty-state" role="status">
          <span className="empty-icon" aria-hidden="true">⌁</span>
          <h3>Chưa có dữ liệu đồ thị</h3>
          <p>Explorer chỉ hiển thị dữ liệu do index service trả về; không tự tạo graph mẫu.</p>
        </div>
      )}
      {!busy && !error && data && data.nodes.length === 0 && (
        <div className="empty-state" role="status">
          <span className="empty-icon" aria-hidden="true">∅</span>
          <h3>Truy vấn không có nút</h3>
          <p>Phản hồi index trả về 0 nút. Kiểm tra provenance bên phải hoặc lập chỉ mục workspace.</p>
        </div>
      )}
      {!busy && !error && data && data.nodes.length > 0 && (
        <div className="graph-workspace">
          <div className="graph-canvas" aria-label="Đồ thị quan hệ từ snapshot đã lập chỉ mục">
            <ReactFlow
              nodes={layout.nodes}
              edges={layout.edges}
              fitView
              fitViewOptions={{ padding: 0.18 }}
              nodesDraggable={false}
              nodesConnectable={false}
              elementsSelectable
              onNodeClick={(_, node) => onSelectNode(node.id)}
              proOptions={{ hideAttribution: true }}
            >
              <Background color="var(--line)" gap={20} />
              <Controls showInteractive={false} />
            </ReactFlow>
          </div>
          <div className="semantic-graph">
            <h3>Danh sách nút có thể truy cập bằng bàn phím</h3>
            <div role="listbox" aria-label="Các nút trong đồ thị">
              {graphNodes.map((node) => (
                <button
                  id={optionId(node.node_id)}
                  className="graph-option"
                  key={node.node_id}
                  role="option"
                  aria-selected={node.node_id === selectedNodeId}
                  tabIndex={node.node_id === (selectedNodeId ?? graphNodes[0]?.node_id) ? 0 : -1}
                  onClick={() => onSelectNode(node.node_id)}
                  onKeyDown={(event) => {
                    if (event.key === "ArrowDown") {
                      event.preventDefault();
                      moveSelection(node.node_id, 1);
                    } else if (event.key === "ArrowUp") {
                      event.preventDefault();
                      moveSelection(node.node_id, -1);
                    } else if (event.key === "Home") {
                      event.preventDefault();
                      moveSelection(node.node_id, "first");
                    } else if (event.key === "End") {
                      event.preventDefault();
                      moveSelection(node.node_id, "last");
                    }
                  }}
                >
                  <span className="node-kind">{node.kind}</span>
                  <span className="node-label">{node.label || node.kind}</span>
                  <span className="node-plane">{planeLabel(node.plane)}</span>
                </button>
              ))}
            </div>
          </div>
          {omittedEdgeCount > 0 && (
            <p className="inline-status" role="status">
              {omittedEdgeCount} quan hệ có đầu mút ngoài subgraph hiện tại nên không được vẽ; phản hồi có thể đã bị giới hạn.
            </p>
          )}
          {selectedNodeId && nodeById.has(selectedNodeId) && <span className="sr-only" role="status">Đã chọn {nodeById.get(selectedNodeId)?.label}</span>}
        </div>
      )}
    </section>
  );
}
