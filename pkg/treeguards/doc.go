// Package treeguards holds structural tests that scan the whole module rather
// than what any one package imports, such as a ban on vendor directories or on
// an interpreter literal in production code.
//
// They live here, not next to the code they protect, because a whole-tree
// test makes PR CI select its package on every change (see the //ci:whole-tree
// marker in scripts/ci/affected). A guard is cheap; the suite of a heavy
// package like agentd or session is not, and it would be dragged along.
package treeguards
