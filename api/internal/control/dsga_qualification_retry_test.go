package control

import (
	"errors"
	"testing"

	"github.com/neondatabase/autoscaling/pkg/dsga/budgetclaims"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func stoppedQualificationFixture() (*batch.Job, *core.Pod) {
	const node = "worker-117"
	const image = "registry.invalid/qualifier@sha256:original"
	controller, started := true, false
	now := metav1.Now()
	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "original-job", Namespace: "neon-control-runtime", UID: "original-job-uid"},
		Spec: batch.JobSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": node},
			Containers:   []core.Container{{Name: "qualifier", Image: image}},
		}}},
		Status: batch.JobStatus{Failed: 1, Conditions: []batch.JobCondition{
			{Type: batch.JobFailed, Status: core.ConditionTrue, Reason: "DeadlineExceeded"},
		}},
	}
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "original-pod", Namespace: job.Namespace, UID: "original-pod-uid", DeletionTimestamp: &now,
			Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)},
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller},
			},
		},
		Spec: core.PodSpec{NodeName: node, Containers: []core.Container{{Name: "qualifier", Image: image}}},
		Status: core.PodStatus{Phase: core.PodFailed, ContainerStatuses: []core.ContainerStatus{
			{Name: "qualifier", Started: &started, State: core.ContainerState{
				Terminated: &core.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"},
			}},
		}},
	}
	return job, pod
}

func TestDSGAQualificationRetryRequiresOriginalTerminalUnstartedPod(t *testing.T) {
	job, pod := stoppedQualificationFixture()
	if !dsgaQualificationStoppedWithoutImage(job, pod) {
		t.Fatal("original failed image pull was not recoverable")
	}
	cases := map[string]func(*batch.Job, *core.Pod){
		"job-not-terminal": func(j *batch.Job, _ *core.Pod) { j.Status.Conditions[0].Type = batch.JobFailureTarget },
		"active":           func(j *batch.Job, _ *core.Pod) { j.Status.Active = 1 },
		"success":          func(j *batch.Job, _ *core.Pod) { j.Status.Succeeded = 1 },
		"foreign-owner":    func(_ *batch.Job, p *core.Pod) { p.OwnerReferences[0].UID = "replacement" },
		"foreign-node":     func(_ *batch.Job, p *core.Pod) { p.Spec.NodeName = "worker-119" },
		"foreign-image":    func(_ *batch.Job, p *core.Pod) { p.Spec.Containers[0].Image = "other" },
		"pulled-image":     func(_ *batch.Job, p *core.Pod) { p.Status.ContainerStatuses[0].ImageID = "digest" },
		"ran": func(_ *batch.Job, p *core.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.Now()
		},
		"previous-run": func(_ *batch.Job, p *core.Pod) {
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &core.ContainerStateTerminated{}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changedJob, changedPod := job.DeepCopy(), pod.DeepCopy()
			mutate(changedJob, changedPod)
			if dsgaQualificationStoppedWithoutImage(changedJob, changedPod) {
				t.Fatal("unconfirmed old execution was admitted")
			}
		})
	}
}

func TestDSGAQualificationRetryPreservesPreviouslyPublishedClaim(t *testing.T) {
	job, pod := stoppedQualificationFixture()
	const ns, vmUID, next = "neon", "original-vm-uid", "original-job-r1"
	claim := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "dsga-q-original", Namespace: ns,
		Labels:          map[string]string{budgetclaims.ClaimLabel: budgetclaims.QualificationKind},
		OwnerReferences: []metav1.OwnerReference{{Name: "original-vm", UID: vmUID}},
	}}
	if _, err := checkDSGAQualificationRetry(t.Context(), fake.NewSimpleClientset(pod, claim), ns, vmUID, job, next); !errors.Is(err, errDSGAQualificationUnconfirmed) {
		t.Fatalf("existing claim should prevent retry: %v", err)
	}
	claim.OwnerReferences[0].UID = "unrelated-vm"
	got, err := checkDSGAQualificationRetry(t.Context(), fake.NewSimpleClientset(pod, claim), ns, vmUID, job, next)
	if err != nil || got != (dsgaQualificationRetry{JobUID: job.UID, PodUID: pod.UID, VMUID: vmUID, NextJob: next}) {
		t.Fatalf("unrelated claim should allow scoped retry: %#v, %v", got, err)
	}
	if _, err = checkDSGAQualificationRetry(t.Context(), fake.NewSimpleClientset(), ns, vmUID, job, next); !errors.Is(err, errDSGAQualificationUnconfirmed) {
		t.Fatalf("missing original Pod should prevent retry: %v", err)
	}
}
