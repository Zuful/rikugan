// Package versioning groups the providers that answer "what changed
// in the code / which commit" questions (concept 5a in
// docs/ontology.md): source-history queries (GitHub, GitLab). Argo CD
// still lives here but answers a deployments question; it moves to
// internal/deployments with its DeploymentSource provider. Runtime →
// commit bridging (get_image_provenance) moved to
// internal/correlation/imageprovenance.
//
// Sub-packages: argocd, github, gitlab. Versioning does
// not yet have a formal Provider interface in pkg/provider — the
// concept stays as a folder grouping for now, with tools registered
// under their backend-specific names. A unified ChangeProvider /
// VersioningProvider interface will land when the abstraction
// is worth designing (likely once a fourth implementation appears).
package versioning
