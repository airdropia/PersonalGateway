// Package contract will hold the pgw endpoint contract tests described in
// plan §14.2 (V1 chat completions shape, streaming SSE, /v1/models
// visibility, admin provider CRUD, discovery parsing, graceful stop, secret
// encryption at rest).
//
// The upstream replay-based contract suites were removed with the vendor
// provider packages in Stage 1 (plan §4.3); the directory stays as the
// home for the new contract suite once the gateway surface in §5.1 is
// wired.
package contract
