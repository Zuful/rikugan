package runes

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// MetricQuery describes a single PromQL query to run.
type MetricQuery struct {
	Name        string
	Description string
	PromQL      string
	Range       queryRange
}

type queryRange struct {
	Start time.Time
	End   time.Time
	Step  time.Duration
}

// MetricResult captures the outcome of a single MetricQuery execution.
type MetricResult struct {
	Query  MetricQuery
	Matrix interface{} // model.Matrix when successful
	Err    error
}

// buildQueries generates PromQL queries based on pod context.
func buildQueries(ns, pod string, podObj *corev1.Pod, start, end time.Time) []MetricQuery {
	step := 60 * time.Second
	r := queryRange{Start: start, End: end, Step: step}

	// Always-generated base queries
	queries := []MetricQuery{
		{
			Name:        "CPU Usage",
			Description: "CPU usage rate per container",
			PromQL:      fmt.Sprintf(`rate(container_cpu_usage_seconds_total{namespace="%s",pod="%s",container!=""}[5m])`, ns, pod),
			Range:       r,
		},
		{
			Name:        "Memory Usage",
			Description: "Working set memory per container",
			PromQL:      fmt.Sprintf(`container_memory_working_set_bytes{namespace="%s",pod="%s",container!=""}`, ns, pod),
			Range:       r,
		},
		{
			Name:        "Restart Count",
			Description: "Total container restarts",
			PromQL:      fmt.Sprintf(`kube_pod_container_status_restarts_total{namespace="%s",pod="%s"}`, ns, pod),
			Range:       r,
		},
	}

	// Conditional: restarts > 0 or OOMKilled detected
	if hasRestartsOrOOM(podObj) {
		queries = append(queries,
			MetricQuery{
				Name:        "OOMKill Events",
				Description: "OOMKill termination events",
				PromQL:      fmt.Sprintf(`kube_pod_container_status_last_terminated_reason{namespace="%s",pod="%s",reason="OOMKilled"}`, ns, pod),
				Range:       r,
			},
			MetricQuery{
				Name:        "Memory Limits",
				Description: "Container memory resource limits",
				PromQL:      fmt.Sprintf(`kube_pod_container_resource_limits{namespace="%s",pod="%s",resource="memory"}`, ns, pod),
				Range:       r,
			},
			MetricQuery{
				Name:        "CPU Throttling",
				Description: "CPU throttle rate per container",
				PromQL:      fmt.Sprintf(`rate(container_cpu_throttled_seconds_total{namespace="%s",pod="%s",container!=""}[5m])`, ns, pod),
				Range:       r,
			},
		)
	}

	// Conditional: pod is Pending
	if podObj.Status.Phase == corev1.PodPending {
		queries = append(queries,
			MetricQuery{
				Name:        "Node Pressure",
				Description: "Nodes under memory, disk, or PID pressure",
				PromQL:      `kube_node_status_condition{condition=~"MemoryPressure|DiskPressure|PIDPressure",status="true"}`,
				Range:       r,
			},
		)
	}

	return queries
}

// hasRestartsOrOOM checks whether any container has restarted or been OOMKilled.
func hasRestartsOrOOM(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > 0 {
			return true
		}
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}
