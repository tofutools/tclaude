// groups-process-scope.js -- which group the run context names for a listed
// group. One node's Groups view lists its groups as the context names them,
// so every group is itself. The fused Groups view lists group@node: a home
// group (this node's, on its own page) resolves to this node's own group,
// and a group whose node's runs are not read here resolves to null -- the
// run context (GET /v1/process/context) is local-only, never served to a
// peer, so a peer's groups show no runs rather than another group's.

import { createContext } from 'preact';
import { useContext } from 'preact/hooks';

const ProcessScope = createContext((group) => group);

export const GroupsProcessScope = ProcessScope.Provider;

// useProcessScope returns the resolver: group -> the context's group, or null.
export function useProcessScope() {
  return useContext(ProcessScope);
}
