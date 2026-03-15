package e2e

import (
	"bytes"
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

func TestAgentDaemonSet(t *testing.T) {
	readiness := features.New("Agent DaemonSet becomes ready").
		Assess("etcd is ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := wait.For(
				conditions.New(cfg.Client().Resources()).DeploymentAvailable("etcd", namespace),
				wait.WithTimeout(2*time.Minute),
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
			}, wait.WithTimeout(3*time.Minute)); err != nil {
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
		Assess("xDS socket exists on node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			assertFileExistsInAgentPod(ctx, t, cfg, "/run/aether/xds.sock")
			return ctx
		}).
		Assess("CNI socket exists on node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			assertFileExistsInAgentPod(ctx, t, cfg, "/run/aether/cni.sock")
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

func assertFileExistsInAgentPod(ctx context.Context, t *testing.T, cfg *envconf.Config, path string) {
	t.Helper()
	pods := listAgentPods(ctx, t, cfg)
	pod := pods.Items[0]

	var stdout, stderr bytes.Buffer
	if err := cfg.Client().Resources().ExecInPod(ctx, pod.Namespace, pod.Name, "agent",
		[]string{"test", "-e", path},
		&stdout, &stderr,
	); err != nil {
		t.Errorf("file %s does not exist in agent pod %s: %v (stderr: %s)", path, pod.Name, err, stderr.String())
	}
}
