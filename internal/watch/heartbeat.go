package watch

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// DefaultHeartbeatInterval is how often the heartbeat is sent when
// RIKUGAN_HEARTBEAT_INTERVAL is not set.
const DefaultHeartbeatInterval = 5 * time.Minute

// Heartbeat pings a dead man's switch (a healthchecks.io check, an
// Uptime Kuma push monitor…) while the daemon's loop runs. A stopped or
// stuck daemon is otherwise silent, and silence reads as "all is well":
// the switch alerts when the pings stop.
type Heartbeat struct {
	// URL receives a GET at every beat. It often carries a secret, so
	// it never appears in logs.
	URL      string
	Interval time.Duration
	Client   *http.Client
	Logger   *slog.Logger
}

func (h *Heartbeat) interval() time.Duration {
	if h.Interval > 0 {
		return h.Interval
	}
	return DefaultHeartbeatInterval
}

// ping sends one beat. Failures are logged and otherwise ignored: the
// switch alerts on the missing beats.
func (h *Heartbeat) ping(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := h.send(ctx)
	if err != nil && ctx.Err() == nil && h.Logger != nil {
		h.Logger.Warn("heartbeat not sent", slog.String("error", err.Error()))
	}
}

func (h *Heartbeat) send(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return fmt.Errorf("heartbeat request: invalid URL")
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req) // #nosec G107 -- the operator's own heartbeat URL
	if err != nil {
		return fmt.Errorf("heartbeat endpoint unreachable") // the transport error names the URL
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat endpoint answered HTTP %d", resp.StatusCode)
	}
	return nil
}
