// Package watch groups the proactive mode's daemon (rikugan serve): what
// it remembers across restarts (journal) and how it decides whether a
// sweep verdict is worth a notification (sweep). The rules that produce
// verdicts live in internal/health and are shared with the
// check_deploy_health tool.
package watch
