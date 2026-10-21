package dispatcher

// Access says whether a tool only reads or changes something.
type Access int

const (
	// Write tools change the systems they reach. The zero value, so
	// that a tool nobody classified counts as one.
	Write Access = iota
	// Read tools only read.
	Read
)

// toolAccess classifies every tool rikugan registers. A tool missing
// here counts as Write: read-only mode withholds it. TestEveryToolIsClassified
// fails the build when a tool is added without an entry.
var toolAccess = map[string]Access{
	// orchestration
	"list_services":             Read,
	"get_service_state":         Read,
	"describe_service":          Read,
	"get_recent_events":         Read,
	"get_service_history":       Read,
	"restart_service":           Write,
	"find_pods_by_service_name": Read,

	// logs
	"get_logs":             Read,
	"search_logs":          Read,
	"tail_logs":            Read,
	"detect_log_anomalies": Read,

	// metrics
	"get_metric":             Read,
	"compare_to_baseline":    Read,
	"list_available_metrics": Read,

	// alerts
	"list_active_alerts":      Read,
	"get_alert_details":       Read,
	"list_issues_for_release": Read,
	"get_current_oncalls":     Read,

	// versioning
	"get_recent_commits":        Read,
	"get_commit_diff":           Read,
	"gitlab_get_recent_commits": Read,
	"gitlab_get_commit_diff":    Read,
	"get_app_status":            Read,
	"list_degraded_apps":        Read,

	// tracing
	"find_slow_traces": Read,
	"analyze_trace":    Read,

	// security
	"get_security_events": Read,
	"analyze_pod_audit":   Read,

	// correlation
	"get_image_provenance": Read,
	"six_eyes":             Read,
	"check_deploy_health":  Read,

	// proactive mode
	"get_watch_reports": Read,
}

// AccessOf returns how a tool was classified; an unclassified tool is
// Write, and ok is false.
func AccessOf(tool string) (a Access, ok bool) {
	a, ok = toolAccess[tool]
	return a, ok
}
