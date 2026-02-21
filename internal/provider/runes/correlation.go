package runes

import (
	"sort"

	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
)

// Severity represents the severity level of a finding.
type Severity int

const (
	INFO Severity = iota
	MEDIUM
	HIGH
	CRITICAL
)

func (s Severity) String() string {
	switch s {
	case INFO:
		return "INFO"
	case MEDIUM:
		return "MEDIUM"
	case HIGH:
		return "HIGH"
	case CRITICAL:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

// Finding represents a single diagnostic signal.
type Finding struct {
	Signal      string
	Severity    Severity
	Explanation string
}

// podSnapshot holds the K8s data needed for correlation.
type podSnapshot struct {
	Pod    *corev1.Pod
	Events []corev1.Event
}

// metricSnapshot holds the Prometheus results needed for correlation.
type metricSnapshot struct {
	Results []MetricResult
}

// correlate applies deterministic heuristics to produce findings.
func correlate(ps podSnapshot, ms metricSnapshot) []Finding {
	var findings []Finding

	pod := ps.Pod

	for _, cs := range pod.Status.ContainerStatuses {
		// OOM Kill detection
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			findings = append(findings, Finding{
				Signal:      "OOM_KILL",
				Severity:    CRITICAL,
				Explanation: "Container " + cs.Name + " was OOMKilled — memory usage exceeded limits.",
			})
			continue
		}

		// Application crash (non-zero exit, not OOM)
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.ExitCode != 0 {
			findings = append(findings, Finding{
				Signal:      "APPLICATION_CRASH",
				Severity:    HIGH,
				Explanation: "Container " + cs.Name + " exited with non-zero code — application error.",
			})
			continue
		}

		// CPU starvation: restarts > 0 AND CPU throttle data present
		if cs.RestartCount > 0 && hasCPUThrottle(ms) {
			findings = append(findings, Finding{
				Signal:      "CPU_STARVATION",
				Severity:    HIGH,
				Explanation: "Container " + cs.Name + " is restarting with CPU throttling detected.",
			})
		}
	}

	// Node pressure: pod Pending AND node pressure metric active
	if pod.Status.Phase == corev1.PodPending && hasNodePressure(ms) {
		findings = append(findings, Finding{
			Signal:      "NODE_PRESSURE",
			Severity:    HIGH,
			Explanation: "Pod is Pending and cluster nodes are under resource pressure.",
		})
	}

	// If no other findings, pod is healthy
	if len(findings) == 0 {
		allReady := true
		for _, cs := range pod.Status.ContainerStatuses {
			if !cs.Ready {
				allReady = false
				break
			}
		}
		if allReady {
			findings = append(findings, Finding{
				Signal:      "HEALTHY",
				Severity:    INFO,
				Explanation: "All containers are ready with no detected issues.",
			})
		}
	}

	// Sort by severity descending
	sort.Slice(findings, func(i, j int) bool {
		return findings[i].Severity > findings[j].Severity
	})

	return findings
}

// hasCPUThrottle checks whether any CPU throttle metric returned non-zero data.
func hasCPUThrottle(ms metricSnapshot) bool {
	for _, r := range ms.Results {
		if r.Query.Name != "CPU Throttling" || r.Err != nil {
			continue
		}
		matrix, ok := r.Matrix.(model.Matrix)
		if !ok {
			continue
		}
		for _, series := range matrix {
			for _, sp := range series.Values {
				if float64(sp.Value) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// hasNodePressure checks whether any node pressure metric returned data.
func hasNodePressure(ms metricSnapshot) bool {
	for _, r := range ms.Results {
		if r.Query.Name != "Node Pressure" || r.Err != nil {
			continue
		}
		matrix, ok := r.Matrix.(model.Matrix)
		if !ok {
			continue
		}
		if len(matrix) > 0 {
			return true
		}
	}
	return false
}
