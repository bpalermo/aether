package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// diagLogTailLines bounds each container's log in a setup-failure dump.
const diagLogTailLines = int64(200)

// diagCallTimeout bounds each API call of a dump on its own, so one stalled
// kubelet log endpoint cannot use up the time of the calls after it.
const diagCallTimeout = 10 * time.Second

// dumpDiagnostics writes the state of the aether-system namespace to stderr:
// every pod with its init and main container states, the namespace's events,
// and the tail of each container's log (and of its previous instance when it
// restarted). The e2e framework runs its Finish actions even when a Setup
// action fails, so DestroyCluster removes the cluster before any CI step after
// the test can look at it: this dump is the only record of why setup failed.
func dumpDiagnostics(cfg *envconf.Config, reason error) {
	ctx := context.Background()
	w := os.Stderr

	fmt.Fprintf(w, "=== e2e setup diagnostics (%v) ===\n", reason)
	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		fmt.Fprintf(w, "diagnostics: no clientset: %v\n", err)
		return
	}

	listCtx, cancelList := context.WithTimeout(ctx, diagCallTimeout)
	pods, err := cs.CoreV1().Pods(namespace).List(listCtx, metav1.ListOptions{})
	cancelList()
	if err != nil {
		fmt.Fprintf(w, "diagnostics: list pods: %v\n", err)
	} else {
		for i := range pods.Items {
			dumpPod(ctx, w, cs, &pods.Items[i])
		}
	}

	eventsCtx, cancelEvents := context.WithTimeout(ctx, diagCallTimeout)
	events, err := cs.CoreV1().Events(namespace).List(eventsCtx, metav1.ListOptions{})
	cancelEvents()
	if err != nil {
		fmt.Fprintf(w, "diagnostics: list events: %v\n", err)
		return
	}
	sort.SliceStable(events.Items, func(i, j int) bool {
		return eventTime(&events.Items[i]).Before(eventTime(&events.Items[j]))
	})
	fmt.Fprintf(w, "--- events in %s ---\n", namespace)
	for i := range events.Items {
		e := &events.Items[i]
		fmt.Fprintf(w, "%s %s %s/%s %s: %s (x%d)\n",
			eventTime(e).UTC().Format(time.RFC3339), e.Type, strings.ToLower(e.InvolvedObject.Kind),
			e.InvolvedObject.Name, e.Reason, e.Message, max(e.Count, 1))
	}
	fmt.Fprintln(w, "=== end of e2e setup diagnostics ===")
}

func dumpPod(ctx context.Context, w io.Writer, cs kubernetes.Interface, pod *corev1.Pod) {
	fmt.Fprintf(w, "--- pod %s: phase=%s node=%s ---\n", pod.Name, pod.Status.Phase, pod.Spec.NodeName)
	for _, c := range pod.Status.Conditions {
		fmt.Fprintf(w, "condition %s=%s %s %s\n", c.Type, c.Status, c.Reason, c.Message)
	}
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, s := range statuses {
		fmt.Fprintf(w, "container %s: ready=%t restarts=%d state=%s last=%s\n",
			s.Name, s.Ready, s.RestartCount, describeState(s.State), describeState(s.LastTerminationState))
		dumpLog(ctx, w, cs, pod.Name, s.Name, false)
		if s.RestartCount > 0 {
			dumpLog(ctx, w, cs, pod.Name, s.Name, true)
		}
	}
}

func dumpLog(ctx context.Context, w io.Writer, cs kubernetes.Interface, pod, container string, previous bool) {
	ctx, cancel := context.WithTimeout(ctx, diagCallTimeout)
	defer cancel()
	tail := diagLogTailLines
	rc, err := cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tail,
	}).Stream(ctx)
	fmt.Fprintf(w, "--- log %s/%s previous=%t (last %d lines) ---\n", pod, container, previous, tail)
	if err != nil {
		fmt.Fprintf(w, "diagnostics: %v\n", err)
		return
	}
	defer rc.Close()
	if _, err := io.Copy(w, rc); err != nil {
		fmt.Fprintf(w, "\ndiagnostics: reading log: %v\n", err)
	}
}

func describeState(s corev1.ContainerState) string {
	switch {
	case s.Running != nil:
		return "running since " + s.Running.StartedAt.UTC().Format(time.RFC3339)
	case s.Waiting != nil:
		return fmt.Sprintf("waiting(%s: %s)", s.Waiting.Reason, s.Waiting.Message)
	case s.Terminated != nil:
		return fmt.Sprintf("terminated(%s, exit %d: %s)", s.Terminated.Reason, s.Terminated.ExitCode, s.Terminated.Message)
	default:
		return "none"
	}
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}
