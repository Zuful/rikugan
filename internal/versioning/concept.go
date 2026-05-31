// Package versioning groups every provider that answers
// "what code is deployed / what changed / which commit" questions:
// GitOps deployment state (Argo CD), source-history queries
// (GitHub, GitLab), runtime → commit bridging (kubernetes image
// provenance).
//
// Sub-packages: argocd, github, gitlab, kubernetes. Versioning does
// not yet have a formal Provider interface in pkg/provider — the
// concept stays as a folder grouping for now, with tools registered
// under their backend-specific names. A unified ChangeProvider /
// VersioningProvider interface will land when the abstraction
// is worth designing (likely once a fourth implementation appears).
package versioning
