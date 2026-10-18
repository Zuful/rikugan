// Command health-survey measures how often check_deploy_health would
// answer "insufficient data" because of the volume guard: for every
// Deployment of a namespace, the share of 5-, 15- and 30-minute windows
// over the last days that hold fewer requests than min_requests.
//
// It is a development tool, not part of the shipped binaries. It reads
// the same settings as the tool (RIKUGAN_HEALTH_CONFIG) and talks to the
// backends through the concept interfaces.
//
//	PROMETHEUS_URL=http://localhost:9090 go run ./scripts/health-survey -namespace prod
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
)

func main() {
	namespace := flag.String("namespace", "", "Kubernetes namespace to survey (required)")
	days := flag.Int("days", 7, "days of history to read")
	flag.Parse()
	if *namespace == "" {
		fmt.Fprintln(os.Stderr, "health-survey: -namespace is required")
		os.Exit(2)
	}
	if err := run(context.Background(), *namespace, time.Duration(*days)*24*time.Hour); err != nil {
		fmt.Fprintln(os.Stderr, "health-survey:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, namespace string, period time.Duration) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	settings, err := health.LoadSettings(os.Getenv("RIKUGAN_HEALTH_CONFIG"))
	if err != nil {
		return err
	}
	api, err := prometheus.BuildAPI("")
	if err != nil {
		return err
	}
	metrics := prometheus.NewFromAPI(api, prometheus.WithLogger(logger))
	client, err := pkgk8s.BuildClient("")
	if err != nil {
		return err
	}
	orch := orchk8s.NewFromClient(client, orchk8s.WithLogger(logger))

	services, err := orch.ListServices(ctx, namespace)
	if err != nil {
		return err
	}

	to := time.Now().Truncate(time.Minute)
	from := to.Add(-period)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "SERVICE\tMIN REQUESTS\tDATA\t+5\t+15\t+30\n")
	for _, svc := range services {
		cfg, tpl := settings.For(svc)
		res, err := health.Survey(ctx, metrics, cfg, tpl, svc, from, to)
		if err != nil {
			fmt.Fprintf(w, "%s\t%.0f\terror: %v\t\t\t\n", svc.Name, cfg.MinRequests, err)
			continue
		}
		fmt.Fprintf(w, "%s\t%.0f\t%s\t%s\t%s\t%s\n", svc.Name, res.MinRequests,
			(time.Duration(res.Minutes) * time.Minute).String(),
			pct(res.Insufficient[5*time.Minute]), pct(res.Insufficient[15*time.Minute]), pct(res.Insufficient[30*time.Minute]))
	}
	fmt.Fprintf(w, "\nShare of windows below the minimum, i.e. how often latency and error rate would be \"insufficient data\" at each checkpoint.\n")
	return w.Flush()
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }
