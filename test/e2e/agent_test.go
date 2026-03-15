package e2e

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const defaultPollInterval = 5 * time.Second

func TestAgentDaemonSet(t *testing.T) {
	readiness := features.New("Agent DaemonSet becomes ready").
		Assess("etcd is ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := wait.For(
				conditions.New(cfg.Client().Resources()).DeploymentAvailable("etcd", namespace),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(defaultPollInterval),
			); err != nil {
				t.Fatalf("etcd deployment not available: %v", err)
			}
			return ctx
		}).
		Assess("agent DaemonSet has all pods ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			client := cfg.Client()

			if err := wait.For(func(ctx context.Context) (bool, error) {
				ds := &appsv1.DaemonSet{}
				if err := client.Resources().Get(ctx, "aether-agent", namespace, ds); err != nil {
					return false, err
				}
				return ds.Status.DesiredNumberScheduled > 0 &&
					ds.Status.NumberReady == ds.Status.DesiredNumberScheduled, nil
			}, wait.WithTimeout(3*time.Minute), wait.WithInterval(defaultPollInterval)); err != nil {
				// Dump pod status on failure to aid debugging.
				pods := &corev1.PodList{}
				if listErr := client.Resources().List(ctx, pods,
					resources.WithLabelSelector("app=aether-agent"),
					resources.WithFieldSelector("metadata.namespace="+namespace),
				); listErr == nil {
					for _, pod := range pods.Items {
						t.Logf("agent pod %s: phase=%s", pod.Name, pod.Status.Phase)
						for _, cs := range pod.Status.ContainerStatuses {
							t.Logf("  container %s: ready=%v restarts=%d", cs.Name, cs.Ready, cs.RestartCount)
							if cs.State.Waiting != nil {
								t.Logf("  waiting: %s - %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
							}
							if cs.State.Terminated != nil {
								t.Logf("  terminated: %s (exit %d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
							}
						}
					}
				}
				t.Fatalf("agent DaemonSet not ready: %v", err)
			}

			return ctx
		}).
		Assess("agent pod is running with no restarts", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			pods := listAgentPods(ctx, t, cfg)
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					t.Errorf("agent pod %s is in phase %s, expected Running", pod.Name, pod.Status.Phase)
				}
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.RestartCount > 0 {
						t.Errorf("agent pod %s container %s has %d restarts", pod.Name, cs.Name, cs.RestartCount)
					}
				}
			}
			return ctx
		}).
		Feature()

	testenv.Test(t, readiness)
}

func listAgentPods(ctx context.Context, t *testing.T, cfg *envconf.Config) *corev1.PodList {
	t.Helper()
	pods := &corev1.PodList{}
	if err := cfg.Client().Resources().List(ctx, pods,
		resources.WithLabelSelector("app=aether-agent"),
		resources.WithFieldSelector("metadata.namespace="+namespace),
	); err != nil {
		t.Fatalf("failed to list agent pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("no agent pods found")
	}
	return pods
}
