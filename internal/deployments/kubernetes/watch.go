package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/Zuful/rikugan/internal/deployments/tracker"
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
		t := tracker.New()
		for p.processNext(ctx, queue, t, out) {
		}
	}()
	return out, nil
}

// processNext reconstructs one queued Deployment's rollout and emits
// what changed. It returns false once the queue is shut down.
func (p *Provider) processNext(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], t *tracker.Tracker, out chan<- provider.Rollout) bool {
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
		t.Forget(key)
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

	for _, ev := range t.Observe(key, r) {
		select {
		case out <- ev:
		case <-ctx.Done():
			return false
		}
	}
	return true
}
