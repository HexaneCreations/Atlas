import { useSyncExternalStore } from "react";

const STORAGE_KEY = "atlas.selected-node";

let current: string | null = readStorage();
const listeners = new Set<() => void>();

function readStorage(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

function notify() {
  for (const l of listeners) l();
}

/** Sets the node every page reads inventory for, until explicitly changed. */
export function setSelectedNodeID(nodeID: string | null) {
  current = nodeID;
  try {
    if (nodeID) localStorage.setItem(STORAGE_KEY, nodeID);
    else localStorage.removeItem(STORAGE_KEY);
  } catch {
    // Storage may be unavailable (private browsing); the in-memory value
    // still works for the current session.
  }
  notify();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(): string | null {
  return current;
}

/**
 * The node id an operator explicitly picked, or null to fall back to the
 * control plane's own node. See [usePrimaryNodeID].
 */
export function useSelectedNodeID(): string | null {
  return useSyncExternalStore(subscribe, getSnapshot);
}
