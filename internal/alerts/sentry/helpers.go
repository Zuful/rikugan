package sentry

// truncate caps a string to max characters, appending an ellipsis if
// the input exceeds the limit. Used by report builders to keep table
// rows narrow.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
