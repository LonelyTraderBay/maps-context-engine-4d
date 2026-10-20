export type InspectorTab = "details" | "source";

export interface SelectionState {
  selectedNodeId: string | null;
  inspectorTab: InspectorTab;
  selectedSourceKey: string | null;
}

export type SelectionAction =
  | { type: "select-node"; nodeId: string }
  | { type: "select-source"; sourceKey: string }
  | { type: "select-tab"; tab: InspectorTab }
  | { type: "clear" };

export const initialSelection: SelectionState = {
  selectedNodeId: null,
  inspectorTab: "details",
  selectedSourceKey: null,
};

export function selectionReducer(state: SelectionState, action: SelectionAction): SelectionState {
  switch (action.type) {
    case "select-node":
      return {
        selectedNodeId: action.nodeId,
        inspectorTab: "details",
        selectedSourceKey: null,
      };
    case "select-source":
      return {
        ...state,
        inspectorTab: "source",
        selectedSourceKey: action.sourceKey,
      };
    case "select-tab":
      return { ...state, inspectorTab: action.tab };
    case "clear":
      return initialSelection;
  }
}

export function sourceRefKey(sourceRef: {
  worktree_id: string;
  profile_id: string;
  generation_id: string;
  path: string;
  content_hash: string;
  range: { start_byte: number; end_byte: number };
}): string {
  const { range } = sourceRef;
  return [
    sourceRef.worktree_id,
    sourceRef.profile_id,
    sourceRef.generation_id,
    sourceRef.path,
    sourceRef.content_hash,
    range.start_byte,
    range.end_byte,
  ].join("\u001f");
}
