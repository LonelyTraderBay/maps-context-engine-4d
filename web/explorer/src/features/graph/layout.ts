import { graphlib, layout } from "@dagrejs/dagre";
import type { Edge, Node } from "@xyflow/react";

import type { GraphData, GraphEdge, GraphNode } from "../../api/types";

export type GraphNodeViewData = Record<string, unknown> & {
  label: string;
  kind: string;
  plane: GraphNode["plane"];
  selected: boolean;
}

const NODE_WIDTH = 232;
const NODE_HEIGHT = 68;

export function layoutGraph(data: GraphData, selectedNodeId: string | null): {
  nodes: Node<GraphNodeViewData>[];
  edges: Edge[];
} {
  const graph = new graphlib.Graph({ directed: true, multigraph: true });
  graph.setGraph({ rankdir: "TB", nodesep: 32, ranksep: 72, marginx: 24, marginy: 24 });
  graph.setDefaultEdgeLabel(() => ({}));

  const sortedNodes = [...data.nodes].sort((left, right) => left.node_id < right.node_id ? -1 : left.node_id > right.node_id ? 1 : 0);
  const sortedEdges = [...data.edges].sort((left, right) => left.edge_id < right.edge_id ? -1 : left.edge_id > right.edge_id ? 1 : 0);
  for (const node of sortedNodes) {
    graph.setNode(node.node_id, { width: NODE_WIDTH, height: NODE_HEIGHT });
  }
  for (const edge of sortedEdges) {
    if (graph.hasNode(edge.from_node_id) && graph.hasNode(edge.to_node_id)) {
      graph.setEdge(edge.from_node_id, edge.to_node_id, {}, edge.edge_id);
    }
  }
  layout(graph);

  const nodes = sortedNodes.flatMap((node) => {
    const position = graph.node(node.node_id) as { x?: number; y?: number } | undefined;
    const x = position?.x;
    const y = position?.y;
    if (typeof x !== "number" || typeof y !== "number" || !Number.isFinite(x) || !Number.isFinite(y)) {
      return [];
    }
    return [{
      id: node.node_id,
      position: { x: x - NODE_WIDTH / 2, y: y - NODE_HEIGHT / 2 },
      data: {
        label: node.label || node.kind,
        kind: node.kind,
        plane: node.plane,
        selected: node.node_id === selectedNodeId,
      },
      style: {
        width: NODE_WIDTH,
        minHeight: NODE_HEIGHT,
        border: node.node_id === selectedNodeId ? "2px solid var(--accent)" : "1px solid var(--line-strong)",
        background: "var(--surface-raised)",
        color: "var(--text-primary)",
        borderRadius: 10,
        padding: 10,
      },
    }];
  });

  const nodeIds = new Set(nodes.map((node) => node.id));
  const edges = sortedEdges.flatMap((edge: GraphEdge) => {
    if (!nodeIds.has(edge.from_node_id) || !nodeIds.has(edge.to_node_id)) {
      return [];
    }
    return [{
      id: edge.edge_id,
      source: edge.from_node_id,
      target: edge.to_node_id,
      label: edge.relation_kind,
      type: "smoothstep",
      markerEnd: { type: "arrowclosed" as const },
      style: { stroke: "var(--line-strong)" },
      labelStyle: { fill: "var(--text-muted)", fontSize: 10 },
    }];
  });
  return { nodes, edges };
}
