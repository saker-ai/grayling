// Package internal is the root of Grayling's private implementation packages.
//
// Layout:
//
//	platform/    control-plane domain store (workspaces, credentials,
//	             drafts, immutable revisions, experiments, events with
//	             unit aggregates, audit, AI ownership index)
//	configstore/ gonacos Config adapter: immutable flag revisions,
//	             manifest publication with read-back verification
//	admin/       RBAC + draft→review→publish pipeline, rollback,
//	             experiment lifecycle operations
//	api/         HTTP surface: /api/v1/admin, /sdk/config, /sdk/eval,
//	             /sdk/stream (SSE), /sdk/ai/*, /collect
//	experiment/  analysis report builder (aggregates → stats → decision)
//	stats/       Welch t-test, two-proportion z, SRM χ², fixed-sample
//	             decision gate (stdlib-only, simulation-tested)
//	ai/          AI resource gate: ownership index, version pinning with
//	             digest re-verification, live revocation, model allowlist
//	gffimport/   strict go-feature-flag importer + legacy-gff-v1 mode
package internal
