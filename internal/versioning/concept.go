// Package versioning groups every provider that answers
// "what code is deployed / what changed / which commit" questions:
// GitOps deployment state (Argo CD), source-history queries
// (GitHub, GitLab), runtime → commit bridging (kubernetes image
// provenance).
//
// Sub-packages: argocd, github, gitlab, kubernetes. Versioning is P1
// in the refactor plan — code is moved into this concept folder
// during Étape 2, but the Provider interface stays informal until
// the P1 work begins. See REFACTOR_PLAN.md.
package versioning
