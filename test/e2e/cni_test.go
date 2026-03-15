package e2e

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestManagedPodRegistration(t *testing.T) {
	feature := features.New("Managed pod is registered in service registry").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := envconf.RandomName("e2e-test", 16)
			ctx = context.WithValue(ctx, testNamespaceKey, ns)

			nsObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			}
			if err := cfg.Client().Resources().Create(ctx, nsObj); err != nil {
				t.Fatalf("failed to create test namespace: %v", err)
			}

			f, err := os.Open("testdata/echo.yaml")
			if err != nil {
				t.Fatalf("failed to open echo.yaml: %v", err)
			}
			defer f.Close()

			objs, err := decoder.DecodeAll(ctx, f)
			if err != nil {
				t.Fatalf("failed to decode echo.yaml: %v", err)
			}
			for _, obj := range objs {
				obj.SetNamespace(ns)
				if err := cfg.Client().Resources().Create(ctx, obj); err != nil {
					t.Fatalf("failed to create %s/%s: %v", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
				}
			}

			return ctx
		}).
		Assess("echo pod becomes ready", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)
			if err := wait.For(
				conditions.New(cfg.Client().Resources()).DeploymentAvailable("echo", ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(defaultPollInterval),
			); err != nil {
				t.Fatalf("echo deployment not available: %v", err)
			}
			return ctx
		}).
		Assess("endpoint is registered in etcd", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)

			pods := &corev1.PodList{}
			if err := cfg.Client().Resources().List(ctx, pods,
				resources.WithLabelSelector("app=echo"),
				resources.WithFieldSelector("metadata.namespace="+ns),
			); err != nil {
				t.Fatalf("failed to list echo pods: %v", err)
			}
			if len(pods.Items) == 0 {
				t.Fatal("no echo pods found")
			}
			podIP := pods.Items[0].Status.PodIP

			etcdPod := getEtcdPod(ctx, t, cfg)

			if err := wait.For(func(_ context.Context) (bool, error) {
				return isIPInEtcd(ctx, cfg, etcdPod, podIP)
			}, wait.WithTimeout(2*time.Minute), wait.WithInterval(defaultPollInterval)); err != nil {
				t.Fatalf("endpoint not found in etcd: %v", err)
			}

			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
			if err := cfg.Client().Resources().Delete(ctx, nsObj); err != nil {
				t.Logf("warning: failed to delete namespace %s: %v", ns, err)
			}
			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}

func TestUnmanagedPodIgnored(t *testing.T) {
	feature := features.New("Unmanaged pod is ignored").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := envconf.RandomName("e2e-test", 16)
			ctx = context.WithValue(ctx, testNamespaceKey, ns)

			nsObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			}
			if err := cfg.Client().Resources().Create(ctx, nsObj); err != nil {
				t.Fatalf("failed to create test namespace: %v", err)
			}

			replicas := int32(1)
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unmanaged",
					Namespace: ns,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "unmanaged"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "unmanaged"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "echo",
									Image: "gcr.io/k8s-staging-gateway-api/echo-basic@sha256:eb7396722956640221e856b8e8cf44179da131d5bd6fc27a4f0cb91f60c237c3",
									Env:   []corev1.EnvVar{{Name: "HTTP_PORT", Value: "8080"}},
									Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("10m"),
										},
									},
								},
							},
							SecurityContext: &corev1.PodSecurityContext{
								RunAsUser:  ptrInt64(65534),
								RunAsGroup: ptrInt64(65534),
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
								},
							},
						},
					},
				},
			}
			if err := cfg.Client().Resources().Create(ctx, dep); err != nil {
				t.Fatalf("failed to create unmanaged deployment: %v", err)
			}

			return ctx
		}).
		Assess("unmanaged pod is ready but not in etcd", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)

			if err := wait.For(
				conditions.New(cfg.Client().Resources()).DeploymentAvailable("unmanaged", ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(defaultPollInterval),
			); err != nil {
				t.Fatalf("unmanaged deployment not available: %v", err)
			}

			pods := &corev1.PodList{}
			if err := cfg.Client().Resources().List(ctx, pods,
				resources.WithLabelSelector("app=unmanaged"),
				resources.WithFieldSelector("metadata.namespace="+ns),
			); err != nil {
				t.Fatalf("failed to list unmanaged pods: %v", err)
			}
			podIP := pods.Items[0].Status.PodIP

			// Wait a bit to allow any registration to happen (if it were going to)
			time.Sleep(10 * time.Second)

			etcdPod := getEtcdPod(ctx, t, cfg)

			var stdout, stderr bytes.Buffer
			if err := cfg.Client().Resources().ExecInPod(ctx, etcdPod.Namespace, etcdPod.Name, "etcd",
				[]string{"etcdctl", "get", "/aether/services/", "--prefix", "--keys-only"},
				&stdout, &stderr,
			); err != nil {
				t.Fatalf("failed to query etcd: %v", err)
			}

			if strings.Contains(stdout.String(), podIP) {
				t.Errorf("unmanaged pod IP %s found in etcd, should not be registered", podIP)
			}

			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
			if err := cfg.Client().Resources().Delete(ctx, nsObj); err != nil {
				t.Logf("warning: failed to delete namespace %s: %v", ns, err)
			}
			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}

func TestPodDeletionCleanup(t *testing.T) {
	feature := features.New("Pod deletion triggers cleanup").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := envconf.RandomName("e2e-test", 16)
			ctx = context.WithValue(ctx, testNamespaceKey, ns)

			nsObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			}
			if err := cfg.Client().Resources().Create(ctx, nsObj); err != nil {
				t.Fatalf("failed to create test namespace: %v", err)
			}

			f, err := os.Open("testdata/echo.yaml")
			if err != nil {
				t.Fatalf("failed to open echo.yaml: %v", err)
			}
			defer f.Close()

			objs, err := decoder.DecodeAll(ctx, f)
			if err != nil {
				t.Fatalf("failed to decode echo.yaml: %v", err)
			}
			for _, obj := range objs {
				obj.SetNamespace(ns)
				if err := cfg.Client().Resources().Create(ctx, obj); err != nil {
					t.Fatalf("failed to create %s/%s: %v", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
				}
			}

			return ctx
		}).
		Assess("echo pod is registered", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)

			if err := wait.For(
				conditions.New(cfg.Client().Resources()).DeploymentAvailable("echo", ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(defaultPollInterval),
			); err != nil {
				t.Fatalf("echo deployment not available: %v", err)
			}

			pods := &corev1.PodList{}
			if err := cfg.Client().Resources().List(ctx, pods,
				resources.WithLabelSelector("app=echo"),
				resources.WithFieldSelector("metadata.namespace="+ns),
			); err != nil {
				t.Fatalf("failed to list echo pods: %v", err)
			}
			podIP := pods.Items[0].Status.PodIP
			ctx = context.WithValue(ctx, testPodIPKey, podIP)

			etcdPod := getEtcdPod(ctx, t, cfg)
			if err := wait.For(func(_ context.Context) (bool, error) {
				return isIPInEtcd(ctx, cfg, etcdPod, podIP)
			}, wait.WithTimeout(2*time.Minute), wait.WithInterval(defaultPollInterval)); err != nil {
				t.Fatalf("endpoint not found in etcd: %v", err)
			}

			return ctx
		}).
		Assess("deleting the deployment removes endpoint from etcd", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)
			podIP := ctx.Value(testPodIPKey).(string)

			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "echo",
					Namespace: ns,
				},
			}
			if err := cfg.Client().Resources().Delete(ctx, dep); err != nil {
				t.Fatalf("failed to delete echo deployment: %v", err)
			}

			etcdPod := getEtcdPod(ctx, t, cfg)
			if err := wait.For(func(_ context.Context) (bool, error) {
				found, err := isIPInEtcd(ctx, cfg, etcdPod, podIP)
				return !found, err
			}, wait.WithTimeout(2*time.Minute), wait.WithInterval(defaultPollInterval)); err != nil {
				t.Fatalf("endpoint %s not cleaned up from etcd: %v", podIP, err)
			}

			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
			if err := cfg.Client().Resources().Delete(ctx, nsObj); err != nil {
				t.Logf("warning: failed to delete namespace %s: %v", ns, err)
			}
			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}

type contextKey string

const (
	testNamespaceKey contextKey = "testNamespace"
	testPodIPKey     contextKey = "testPodIP"
)

func getEtcdPod(ctx context.Context, t *testing.T, cfg *envconf.Config) corev1.Pod {
	t.Helper()
	etcdPods := &corev1.PodList{}
	if err := cfg.Client().Resources().List(ctx, etcdPods,
		resources.WithLabelSelector("app=etcd"),
		resources.WithFieldSelector("metadata.namespace="+namespace),
	); err != nil {
		t.Fatalf("failed to list etcd pods: %v", err)
	}
	if len(etcdPods.Items) == 0 {
		t.Fatal("no etcd pods found")
	}
	return etcdPods.Items[0]
}

func isIPInEtcd(ctx context.Context, cfg *envconf.Config, etcdPod corev1.Pod, podIP string) (bool, error) {
	var stdout, stderr bytes.Buffer
	err := cfg.Client().Resources().ExecInPod(ctx, etcdPod.Namespace, etcdPod.Name, "etcd",
		[]string{"etcdctl", "get", "/aether/services/", "--prefix", "--keys-only"},
		&stdout, &stderr,
	)
	if err != nil {
		return false, nil
	}
	return strings.Contains(stdout.String(), podIP), nil
}
