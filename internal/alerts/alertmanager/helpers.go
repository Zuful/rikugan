package alertmanager

// defaultString returns s when non-empty, else fallback. Used by the
// AlertsProvider projection to surface a sentinel when labels are
// missing.
func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
