package e2e

import (
	"context"
	"os"
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
	feature := features.New("Managed pod is discoverable by Kubernetes registry").
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
		Assess("managed pod is running with PodIP and managed label", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)

			pods := &corev1.PodList{}
			if err := cfg.Client().Resources().List(ctx, pods,
				resources.WithLabelSelector("app=echo,aether.io/managed=true"),
				resources.WithFieldSelector("metadata.namespace="+ns),
			); err != nil {
				t.Fatalf("failed to list managed echo pods: %v", err)
			}
			if len(pods.Items) == 0 {
				t.Fatal("no managed echo pods found with aether.io/managed=true label")
			}

			pod := pods.Items[0]
			if pod.Status.Phase != corev1.PodRunning {
				t.Fatalf("managed pod %s is in phase %s, expected Running", pod.Name, pod.Status.Phase)
			}
			if pod.Status.PodIP == "" {
				t.Fatalf("managed pod %s has no PodIP", pod.Name)
			}
			t.Logf("managed pod %s is Running with PodIP %s and aether.io/managed=true label", pod.Name, pod.Status.PodIP)

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
		Assess("unmanaged pod is ready but not discoverable as managed", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)

			if err := wait.For(
				conditions.New(cfg.Client().Resources()).DeploymentAvailable("unmanaged", ns),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(defaultPollInterval),
			); err != nil {
				t.Fatalf("unmanaged deployment not available: %v", err)
			}

			// Verify the unmanaged pod does NOT have the aether.io/managed label
			pods := &corev1.PodList{}
			if err := cfg.Client().Resources().List(ctx, pods,
				resources.WithLabelSelector("app=unmanaged"),
				resources.WithFieldSelector("metadata.namespace="+ns),
			); err != nil {
				t.Fatalf("failed to list unmanaged pods: %v", err)
			}
			if len(pods.Items) == 0 {
				t.Fatal("no unmanaged pods found")
			}

			for _, pod := range pods.Items {
				if val, ok := pod.Labels["aether.io/managed"]; ok && val == "true" {
					t.Errorf("unmanaged pod %s has aether.io/managed=true label, should not be managed", pod.Name)
				}
			}

			// Verify no pods with aether.io/managed=true exist in this namespace
			managedPods := &corev1.PodList{}
			if err := cfg.Client().Resources().List(ctx, managedPods,
				resources.WithLabelSelector("aether.io/managed=true"),
				resources.WithFieldSelector("metadata.namespace="+ns),
			); err != nil {
				t.Fatalf("failed to list managed pods: %v", err)
			}
			if len(managedPods.Items) > 0 {
				t.Errorf("found %d managed pods in namespace %s, expected 0", len(managedPods.Items), ns)
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
	feature := features.New("Pod deletion removes managed pod").
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
		Assess("echo pod is running with managed label", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
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
				resources.WithLabelSelector("app=echo,aether.io/managed=true"),
				resources.WithFieldSelector("metadata.namespace="+ns),
			); err != nil {
				t.Fatalf("failed to list managed echo pods: %v", err)
			}
			if len(pods.Items) == 0 {
				t.Fatal("no managed echo pods found")
			}

			pod := pods.Items[0]
			if pod.Status.PodIP == "" {
				t.Fatalf("managed pod %s has no PodIP", pod.Name)
			}
			t.Logf("managed pod %s running with PodIP %s", pod.Name, pod.Status.PodIP)

			return ctx
		}).
		Assess("deleting the deployment removes managed pods", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(testNamespaceKey).(string)

			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "echo",
					Namespace: ns,
				},
			}
			if err := cfg.Client().Resources().Delete(ctx, dep); err != nil {
				t.Fatalf("failed to delete echo deployment: %v", err)
			}

			// Wait for all managed pods to be gone from the namespace
			if err := wait.For(func(ctx context.Context) (bool, error) {
				pods := &corev1.PodList{}
				if err := cfg.Client().Resources().List(ctx, pods,
					resources.WithLabelSelector("app=echo,aether.io/managed=true"),
					resources.WithFieldSelector("metadata.namespace="+ns),
				); err != nil {
					return false, err
				}
				return len(pods.Items) == 0, nil
			}, wait.WithTimeout(2*time.Minute), wait.WithInterval(defaultPollInterval)); err != nil {
				t.Fatalf("managed pods not cleaned up after deployment deletion: %v", err)
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
)
