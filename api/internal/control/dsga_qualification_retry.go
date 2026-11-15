package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/neondatabase/autoscaling/pkg/dsga/budgetclaims"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

var (
	errDSGAQualificationUnconfirmed = errors.New("original qualification may have executed or published a claim")
	errDSGAQualificationJobFailed   = errors.New("original qualification job failed")
)

type dsgaQualificationRetry struct {
	JobUID  types.UID `json:"job_uid"`
	PodUID  types.UID `json:"pod_uid"`
	VMUID   types.UID `json:"vm_uid"`
	NextJob string    `json:"next_job"`
}

func dsgaQualificationJobFailed(job *batch.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batch.JobFailed && condition.Status == core.ConditionTrue {
			return true
		}
	}
	return false
}

func dsgaQualificationStoppedWithoutImage(job *batch.Job, pod *core.Pod) bool {
	if job.Status.Active != 0 || job.Status.Succeeded != 0 || job.Status.Failed != 1 ||
		(job.Status.Terminating != nil && *job.Status.Terminating != 0) {
		return false
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type != batch.JobFailed || condition.Status != core.ConditionTrue {
			continue
		}
		legacyDeadline := job.Spec.Template.Spec.ActiveDeadlineSeconds == nil && condition.Reason == batch.JobReasonDeadlineExceeded
		kubeletDeadline := job.Spec.ActiveDeadlineSeconds == nil && job.Spec.Template.Spec.ActiveDeadlineSeconds != nil &&
			condition.Reason == batch.JobReasonBackoffLimitExceeded && pod.Status.Reason == "DeadlineExceeded"
		if (!legacyDeadline && !kubeletDeadline) || len(job.Spec.Template.Spec.Containers) != 1 {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.UID != job.UID ||
			owner.Name != job.Name || pod.UID == "" || (legacyDeadline && pod.DeletionTimestamp == nil) || pod.Status.Phase != core.PodFailed ||
			pod.Spec.NodeName != job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] ||
			len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 ||
			pod.Spec.Containers[0].Name != "qualifier" || pod.Spec.Containers[0].Image != job.Spec.Template.Spec.Containers[0].Image ||
			len(pod.Status.ContainerStatuses) != 1 || len(pod.Status.InitContainerStatuses) != 0 ||
			len(pod.Status.EphemeralContainerStatuses) != 0 {
			return false
		}
		status := pod.Status.ContainerStatuses[0]
		end := status.State.Terminated
		return status.Name == "qualifier" && status.Started != nil && !*status.Started && !status.Ready &&
			status.ImageID == "" && status.ContainerID == "" && status.RestartCount == 0 &&
			status.LastTerminationState == (core.ContainerState{}) && end != nil &&
			end.Reason == "ContainerStatusUnknown" && end.ContainerID == "" && end.StartedAt.IsZero() && end.FinishedAt.IsZero()
	}
	return false
}

// A terminal Job alone does not prove the qualifier left no output. A publisher
// may commit a protected claim and lose its reply. Preserve that identity for
// reconciliation instead of starting another qualifier.
func checkDSGAQualificationRetry(ctx context.Context, clients kubernetes.Interface, ns string, vmUID types.UID, job *batch.Job, nextJob string) (dsgaQualificationRetry, error) {
	var retry dsgaQualificationRetry
	pods, err := clients.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{"batch.kubernetes.io/controller-uid": string(job.UID)}.String(),
	})
	if err != nil {
		return retry, err
	}
	if len(pods.Items) != 1 || !dsgaQualificationStoppedWithoutImage(job, &pods.Items[0]) {
		return retry, errDSGAQualificationUnconfirmed
	}
	if err = verifyNoDSGAQualificationClaim(ctx, clients, ns, vmUID); err != nil {
		return retry, err
	}
	return dsgaQualificationRetry{JobUID: job.UID, PodUID: pods.Items[0].UID, VMUID: vmUID, NextJob: nextJob}, nil
}

func verifyNoDSGAQualificationClaim(ctx context.Context, clients kubernetes.Interface, namespace string, vmUID types.UID) error {
	options := metav1.ListOptions{
		LabelSelector: labels.Set{budgetclaims.ClaimLabel: budgetclaims.QualificationKind}.String(),
		Limit:         128,
	}
	for {
		out, err := clients.CoreV1().ConfigMaps(namespace).List(ctx, options)
		if err != nil {
			return err
		}
		for _, cm := range out.Items {
			for _, owner := range cm.OwnerReferences {
				if owner.UID == vmUID {
					return errDSGAQualificationUnconfirmed
				}
			}
		}
		if out.Continue == "" {
			break
		}
		options.Continue = out.Continue
	}
	return nil
}

func dsgaQualificationRetryKinds(attempt int) (string, string) {
	return fmt.Sprintf("qualification_retry_%d", attempt), fmt.Sprintf("qualification_job_retry_%d", attempt)
}

// Pre-upgrade Jobs keep their original Kubernetes contract and recorded Pod
// digest. A retry Job may use the new execution deadline while the old Job
// remains available for reconciliation.
func matchDSGAQualificationJob(got, wanted *batch.Job) (*batch.Job, bool) {
	if sameDSGAQualificationJob(got, wanted) {
		return wanted, true
	}
	legacy := wanted.DeepCopy()
	legacy.Spec.ActiveDeadlineSeconds = legacy.Spec.Template.Spec.ActiveDeadlineSeconds
	legacy.Spec.Template.Spec.ActiveDeadlineSeconds = nil
	return legacy, sameDSGAQualificationJob(got, legacy)
}

// Retain the first Job and all recovery Jobs. A retry after an unknown write
// response must recover that attempt or stop on identity drift.
func (b *dsgaCreationBinding) selectDSGAQualificationJob(ctx context.Context, clients kubernetes.Interface, vmNamespace string, vmUID types.UID, first *batch.Job) (*batch.Job, *batch.Job, error) {
	wanted, kind := first, "qualification_job"
	jobs := clients.BatchV1().Jobs(first.Namespace)
	for attempt := 1; ; attempt++ {
		rawFact, err := b.supplyFact(ctx, kind, nil)
		if err != nil {
			return nil, nil, err
		}
		var original dsgaSupplyObject
		if rawFact != nil && json.Unmarshal(rawFact, &original) != nil {
			return nil, nil, errDSGACreationIdentity
		}
		actual, err := jobs.Get(ctx, wanted.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && rawFact == nil {
			actual = &batch.Job{}
			err = clients.BatchV1().RESTClient().Post().Namespace(wanted.Namespace).Resource("jobs").Body(wanted).MaxRetries(0).Do(ctx).Into(actual)
		}
		if err != nil {
			return nil, nil, err
		}
		matched, same := matchDSGAQualificationJob(actual, wanted)
		if !same || (rawFact != nil && original.UID != string(actual.UID)) {
			return nil, nil, errDSGACreationIdentity
		}
		spec, err := json.Marshal(matched.Spec.Template.Spec)
		if err != nil {
			return nil, nil, err
		}
		if _, err = b.supplyFact(ctx, kind, dsgaSupplyObject{actual.Name, string(actual.UID), managedSQLDigest(spec)}); err != nil {
			return nil, nil, err
		}
		if !dsgaQualificationJobFailed(actual) {
			return actual, matched, nil
		}

		retryKind, nextKind := dsgaQualificationRetryKinds(attempt)
		next := first.DeepCopy()
		next.Name = fmt.Sprintf("%s-r%d", first.Name, attempt)
		decision, err := b.supplyFact(ctx, retryKind, nil)
		if err != nil {
			return nil, nil, err
		}
		if decision != nil {
			var retry dsgaQualificationRetry
			if json.Unmarshal(decision, &retry) != nil || retry.JobUID != actual.UID || retry.PodUID == "" ||
				retry.VMUID != vmUID || retry.NextJob != next.Name {
				return nil, nil, errDSGACreationIdentity
			}
		} else {
			result, err := b.supplyFact(ctx, "qualification_result", nil)
			if err != nil {
				return nil, nil, err
			}
			if result != nil {
				return nil, nil, errDSGAQualificationUnconfirmed
			}
			retry, err := checkDSGAQualificationRetry(ctx, clients, vmNamespace, vmUID, actual, next.Name)
			if err != nil {
				return nil, nil, err
			}
			if _, err = b.supplyFact(ctx, retryKind, retry); err != nil {
				return nil, nil, err
			}
		}
		wanted, kind = next, nextKind
	}
}
