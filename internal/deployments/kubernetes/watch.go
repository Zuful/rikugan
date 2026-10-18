package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/Zuful/rikugan/pkg/provider"
)

// resyncPeriod replays every Deployment through the queue now and then,
// so a rollout whose last event was dropped (an error past the retry
// budget) is still reported. Already-reported states are deduplicated.
const resyncPeriod = 10 * time.Minute

// maxRetries bounds how often one Deployment is retried after a failed
// reconstruction before it waits for its next event or resync.
const maxRetries = 8

// WithNamespaces scopes WatchRollouts to the given namespaces. Without
// it, every namespace is watched, which needs cluster-wide list and
// watch rights on Deployments.
func (p *Provider) WithNamespaces(namespaces ...string) *Provider {
	p.namespaces = namespaces
	return p
}

// WatchRollouts implements provider.DeploymentSource.
//
// A Deployment informer is used only as a trigger: every add or update
// queues the Deployment, and the rollout is then reconstructed by
// LastRollout, the same code path as check_deploy_health, validated on a
// real controller (rollout_integration_test.go). The watcher never
// stamps dates of its own.
func (p *Provider) WatchRollouts(ctx context.Context) (<-chan provider.Rollout, error) {
	namespaces := p.namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""} // all namespaces
	}

	queue := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "rollouts"},
	)
	enqueue := func(obj any) {
		if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
			queue.Add(key)
		}
	}

	var synced []cache.InformerSynced
	for _, ns := range namespaces {
		factory := informers.NewSharedInformerFactoryWithOptions(p.client, resyncPeriod, informers.WithNamespace(ns))
		inf := factory.Apps().V1().Deployments().Informer()
		if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    enqueue,
			UpdateFunc: func(_, obj any) { enqueue(obj) },
			// A deleted Deployment is queued too: its reconstruction
			// fails with NotFound, which drops its tracking.
			DeleteFunc: func(obj any) {
				if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
					queue.Add(key)
				}
			},
		}); err != nil {
			queue.ShutDown()
			return nil, fmt.Errorf("watch deployments: %w", err)
		}
		factory.Start(ctx.Done())
		synced = append(synced, inf.HasSynced)
	}

	out := make(chan provider.Rollout)
	go func() {
		<-ctx.Done()
		queue.ShutDown()
	}()
	go func() {
		defer close(out)
		if !cache.WaitForCacheSync(ctx.Done(), synced...) {
			return
		}
		t := newTracker()
		for p.processNext(ctx, queue, t, out) {
		}
	}()
	return out, nil
}

// processNext reconstructs one queued Deployment's rollout and emits
// what changed. It returns false once the queue is shut down.
func (p *Provider) processNext(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], t *tracker, out chan<- provider.Rollout) bool {
	key, shutdown := queue.Get()
	if shutdown {
		return false
	}
	defer queue.Done(key)

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		queue.Forget(key)
		return true
	}
	r, err := p.LastRollout(ctx, provider.Service{Name: name, Namespace: ns, Kind: "deployment"})
	switch {
	case apierrors.IsNotFound(err):
		t.forget(key)
		queue.Forget(key)
		return true
	case err != nil:
		// Typically a new revision whose ReplicaSet is not created yet:
		// retry shortly, then wait for the next event or resync.
		if queue.NumRequeues(key) < maxRetries {
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
			p.logger.Warn("rollout reconstruction keeps failing", slog.String("deployment", key), slog.String("error", err.Error()))
		}
		return true
	}
	queue.Forget(key)

	for _, ev := range t.observe(key, r) {
		select {
		case out <- ev:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// tracker remembers, per Deployment, the last rollout seen and the
// states already reported, so each (sequence, state) is emitted once.
type tracker struct {
	last map[string]tracked
}

type tracked struct {
	rollout  provider.Rollout
	reported map[provider.RolloutState]bool
}

func newTracker() *tracker { return &tracker{last: map[string]tracked{}} }

func (t *tracker) forget(key string) { delete(t.last, key) }

// observe takes the Deployment's current rollout and returns what to
// report, oldest first:
//   - the previous rollout, as superseded, if a newer one started before
//     it ended;
//   - a superseded rollout for every revision number skipped between two
//     observations: two rollouts can start before the first event is
//     processed, and the reconstruction only ever sees the last one;
//   - the current rollout, if it has ended in a state not reported yet.
func (t *tracker) observe(key string, r provider.Rollout) []provider.Rollout {
	var out []provider.Rollout
	prev, seen := t.last[key]
	if seen && prev.rollout.Sequence != r.Sequence {
		if !prev.ended() {
			s := prev.rollout
			s.State = provider.RolloutSuperseded
			out = append(out, s)
		}
		out = append(out, skipped(prev.rollout, r)...)
		prev = tracked{}
	}
	if prev.reported == nil {
		prev.reported = map[provider.RolloutState]bool{}
	}
	prev.rollout = r
	if (r.State == provider.RolloutComplete || r.State == provider.RolloutFailed) && !prev.reported[r.State] {
		prev.reported[r.State] = true
		out = append(out, r)
	}
	t.last[key] = prev
	return out
}

// ended reports whether the rollout completed or failed: a newer one
// does not supersede it.
func (tr tracked) ended() bool {
	return tr.reported[provider.RolloutComplete] || tr.reported[provider.RolloutFailed] ||
		tr.rollout.State == provider.RolloutComplete || tr.rollout.State == provider.RolloutFailed
}

// skipped returns a superseded rollout for each revision number strictly
// between prev and cur. Only the service and the sequence are known: the
// reconstruction never saw them.
func skipped(prev, cur provider.Rollout) []provider.Rollout {
	from, err1 := strconv.Atoi(prev.Sequence)
	to, err2 := strconv.Atoi(cur.Sequence)
	if err1 != nil || err2 != nil || to-from > 100 { // a gap that large is not a burst of rollouts
		return nil
	}
	var out []provider.Rollout
	for n := from + 1; n < to; n++ {
		out = append(out, provider.Rollout{Service: cur.Service, Sequence: strconv.Itoa(n), State: provider.RolloutSuperseded})
	}
	return out
}
