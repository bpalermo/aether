package e2e

import (
	"context"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	testenv         env.Environment
	kindClusterName = "aether-e2e"
	namespace       = "aether-system"

	agentImage     = envOrDefault("AETHER_AGENT_IMAGE", "palermo/aether-agent:latest")
	cniInstallImage = envOrDefault("AETHER_CNI_INSTALL_IMAGE", "palermo/aether-cni-install:latest")
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestMain(m *testing.M) {
	testenv = env.New()

	kindProvider := kind.NewProvider()

	testenv.Setup(
		envfuncs.CreateClusterWithConfig(kindProvider, kindClusterName, "testdata/kind-config.yaml"),
		envfuncs.LoadDockerImageToCluster(kindClusterName, agentImage),
		envfuncs.LoadDockerImageToCluster(kindClusterName, cniInstallImage),
		deployAetherSystem(),
	)

	testenv.Finish(
		envfuncs.DestroyCluster(kindClusterName),
	)

	os.Exit(testenv.Run(m))
}

func deployAetherSystem() env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		client := cfg.Client()

		// Create aether-system namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Labels: map[string]string{
					"pod-security.kubernetes.io/audit":   "privileged",
					"pod-security.kubernetes.io/enforce": "privileged",
					"pod-security.kubernetes.io/warn":    "privileged",
				},
			},
		}
		if err := client.Resources().Create(ctx, ns); err != nil {
			return ctx, err
		}

		// Create ServiceAccount
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "aether-agent",
				Namespace: namespace,
			},
		}
		if err := client.Resources().Create(ctx, sa); err != nil {
			return ctx, err
		}

		// Create ClusterRole
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "aether-agent",
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{""},
					Resources: []string{"pods"},
					Verbs:     []string{"list", "get", "watch"},
				},
				{
					APIGroups: []string{""},
					Resources: []string{"nodes"},
					Verbs:     []string{"list", "get", "watch"},
				},
			},
		}
		if err := client.Resources().Create(ctx, cr); err != nil {
			return ctx, err
		}

		// Create ClusterRoleBinding
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "aether-agent",
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "aether-agent",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      "aether-agent",
					Namespace: namespace,
				},
			},
		}
		if err := client.Resources().Create(ctx, crb); err != nil {
			return ctx, err
		}

		// Deploy etcd for registry backend
		if err := deployEtcd(ctx, client); err != nil {
			return ctx, err
		}

		// Deploy agent DaemonSet
		if err := deployAgent(ctx, client); err != nil {
			return ctx, err
		}

		return ctx, nil
	}
}

func deployEtcd(ctx context.Context, client klient.Client) error {
	labels := map[string]string{"app": "etcd"}
	replicas := int32(1)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "etcd",
							Image: "quay.io/coreos/etcd:v3.5.21",
							Command: []string{
								"etcd",
								"--advertise-client-urls=http://0.0.0.0:2379",
								"--listen-client-urls=http://0.0.0.0:2379",
							},
							Ports: []corev1.ContainerPort{
								{ContainerPort: 2379, Name: "client"},
							},
						},
					},
				},
			},
		},
	}
	if err := client.Resources().Create(ctx, dep); err != nil {
		return err
	}

	// Create etcd Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd",
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Port: 2379, Name: "client"},
			},
		},
	}
	return client.Resources().Create(ctx, svc)
}

func deployAgent(ctx context.Context, client klient.Client) error {
	labels := map[string]string{
		"app":                    "aether-agent",
		"app.kubernetes.io/name": "aether-agent",
	}
	hostPathDirOrCreate := corev1.HostPathDirectoryOrCreate

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aether-agent",
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: "aether-agent",
					InitContainers: []corev1.Container{
						{
							Name:            "cni-install",
							Image:           cniInstallImage,
							ImagePullPolicy: corev1.PullNever,
							Args:            []string{"--debug=true"},
							SecurityContext: &corev1.SecurityContext{
								ReadOnlyRootFilesystem:   ptrBool(true),
								AllowPrivilegeEscalation: ptrBool(false),
								RunAsUser:                ptrInt64(0),
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "cni-bin-dir", MountPath: "/host/opt/cni/bin"},
								{Name: "cni-conf-dir", MountPath: "/host/etc/cni/net.d"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "agent",
							Image:           agentImage,
							ImagePullPolicy: corev1.PullNever,
							Args: []string{
								"--debug=true",
								"--proxy-id=$(NODE_NAME)",
								"--cluster-name=aether-e2e",
								"--node-name=$(NODE_NAME)",
								"--registry-backend=etcd",
								"--etcd-endpoints=etcd.aether-system.svc.cluster.local:2379",
							},
							Env: []corev1.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "spec.nodeName",
										},
									},
								},
							},
							SecurityContext: &corev1.SecurityContext{
								ReadOnlyRootFilesystem:   ptrBool(true),
								AllowPrivilegeEscalation: ptrBool(false),
								RunAsUser:                ptrInt64(0),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
									Add:  []corev1.Capability{"NET_ADMIN"},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "run-dir", MountPath: "/run/aether", MountPropagation: ptrMountPropagation(corev1.MountPropagationHostToContainer)},
								{Name: "cni-netns-dir", MountPath: "/host/var/run/netns", MountPropagation: ptrMountPropagation(corev1.MountPropagationHostToContainer)},
								{Name: "cni-registry-dir", MountPath: "/host/var/lib/aether/registry", MountPropagation: ptrMountPropagation(corev1.MountPropagationHostToContainer)},
								{Name: "tmp", MountPath: "/tmp"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "run-dir", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/aether", Type: &hostPathDirOrCreate}}},
						{Name: "cni-bin-dir", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/cni/bin", Type: &hostPathDirOrCreate}}},
						{Name: "cni-conf-dir", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/cni/net.d", Type: &hostPathDirOrCreate}}},
						{Name: "cni-netns-dir", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/netns", Type: &hostPathDirOrCreate}}},
						{Name: "cni-registry-dir", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/aether/registry", Type: &hostPathDirOrCreate}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	return client.Resources().Create(ctx, ds)
}

func ptrBool(b bool) *bool                                                           { return &b }
func ptrInt64(i int64) *int64                                                        { return &i }
func ptrMountPropagation(m corev1.MountPropagationMode) *corev1.MountPropagationMode { return &m }
