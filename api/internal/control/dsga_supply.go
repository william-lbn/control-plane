package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/budgetclaims"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/qualificationissuer"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// 原Operation内保存外部UID/签发结果；不分配预算，不改变伸缩WAL。写前/提交前
// 均核原worker lease；未知COMMIT由同一kind/完整事实读回，不覆写既有事实。
func (b *dsgaCreationBinding) supplyFact(ctx context.Context, kind string, value any) ([]byte, error) {
	tx, err := b.lockOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, "SELECT value FROM dsga_creation_supply_facts WHERE operation_id=$1 AND kind=$2", b.operationID, kind).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) && value == nil {
		return nil, nil
	}
	if value != nil {
		next, e := json.Marshal(value)
		if e != nil || len(next) > 64<<10 {
			return nil, errDSGACreationIdentity
		}
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, "INSERT INTO dsga_creation_supply_facts(operation_id,kind,value) VALUES($1,$2,$3)", b.operationID, kind, next)
			raw = next
		} else if err == nil {
			var a, c any
			if json.Unmarshal(raw, &a) != nil || json.Unmarshal(next, &c) != nil || !reflect.DeepEqual(a, c) {
				return nil, errDSGACreationIdentity
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = b.checkLease(ctx, tx); err != nil {
		return nil, err
	}
	return raw, tx.Commit(ctx)
}

type dsgaSupplyObject struct {
	Name   string `json:"name"`
	UID    string `json:"uid"`
	SHA256 string `json:"sha256"`
}

// Job无shell、无宿主PID/特权，不拿业务凭据；只连接同节点原Agent只读UDS。
// backoff=0 + Never防止不确定签发被自动重放。新Job名绑定原VM UID与PG boot。
func dsgaQualificationJob(vm *vmv1.VirtualMachine, plan dsgaCreationPlan, nodeUID string, boot dsgapi.Uint64) (*core.ConfigMap, *batch.Job, error) {
	s := plan.VM.supply()
	if s == nil || s.validate() != nil || vm == nil || vm.UID == "" || vm.Status.Node == "" || boot == 0 {
		return nil, nil, errDSGACreationIdentity
	}
	name := "dsga-qjob-" + managedSQLDigest([]byte(string(vm.UID) + "/" + fmt.Sprint(boot)))[:40]
	cfg := qualificationissuer.CollectorConfig{ProtocolVersion: 1, Binding: journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: plan.Incarnation}, NodeName: vm.Status.Node, NodeUID: nodeUID,
		ControlSocketPath: "/run/dsga-resource/" + s.ResourceReadSocketName, TimeoutSeconds: 120, OutputDirectory: "/out/evidence", Targets: s.Targets}
	if cfg.Validate() != nil {
		return nil, nil, errDSGACreationPlan
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}
	yes, no, zero := true, false, int32(0)
	root := int64(0)
	deadline := int64(300)
	mode := int32(0400)
	directory := core.HostPathDirectory
	labels := map[string]string{dsgaSupplyVMLabel: string(vm.UID)}
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.PlatformNamespace, Labels: labels}, Immutable: &yes, Data: map[string]string{"collector.json": string(raw)}}
	pod := core.PodSpec{SchedulerName: plan.VM.SchedulerName, NodeSelector: map[string]string{"kubernetes.io/hostname": vm.Status.Node}, ServiceAccountName: s.QualifierServiceAccount, RestartPolicy: core.RestartPolicyNever, AutomountServiceAccountToken: &yes,
		SecurityContext: &core.PodSecurityContext{RunAsUser: &root, RunAsGroup: &root, SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}},
		Containers: []core.Container{{Name: "qualifier", Image: s.QualifierImage, ImagePullPolicy: core.PullIfNotPresent, Args: []string{"--collect-publish-config", "/config/collector.json"},
			SecurityContext: &core.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}},
			Resources:       core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("250m"), core.ResourceMemory: resource.MustParse("256Mi")}, Limits: core.ResourceList{core.ResourceCPU: resource.MustParse("250m"), core.ResourceMemory: resource.MustParse("256Mi")}},
			VolumeMounts:    []core.VolumeMount{{Name: "config", MountPath: "/config/collector.json", SubPath: "collector.json", ReadOnly: true}, {Name: "output", MountPath: "/out"}, {Name: "root-read", MountPath: "/run/dsga-resource", ReadOnly: true}}}},
		Volumes: []core.Volume{{Name: "config", VolumeSource: core.VolumeSource{ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: name}, DefaultMode: &mode}}},
			{Name: "output", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(32<<20, resource.BinarySI)}}},
			{Name: "root-read", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: s.ResourceReadHostDirectory, Type: &directory}}}}}
	job := &batch.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.PlatformNamespace, Labels: labels}, Spec: batch.JobSpec{BackoffLimit: &zero, ActiveDeadlineSeconds: &deadline, Template: core.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: pod}}}
	return cm, job, nil
}

func sameDSGAQualificationJob(got, wanted *batch.Job) bool {
	if got == nil || wanted == nil || got.UID == "" || got.ResourceVersion == "" || got.DeletionTimestamp != nil || got.Name != wanted.Name || got.Namespace != wanted.Namespace || got.Labels[dsgaSupplyVMLabel] != wanted.Labels[dsgaSupplyVMLabel] ||
		!reflect.DeepEqual(got.Spec.BackoffLimit, wanted.Spec.BackoffLimit) || !reflect.DeepEqual(got.Spec.ActiveDeadlineSeconds, wanted.Spec.ActiveDeadlineSeconds) {
		return false
	}
	// Kubernetes默认字段合法；仅投影已显式冻结的完整执行合同，额外容器/卷仍拒绝。
	a, b := got.Spec.Template.Spec, wanted.Spec.Template.Spec
	if a.NodeName != b.NodeName || a.SchedulerName != b.SchedulerName || !reflect.DeepEqual(a.NodeSelector, b.NodeSelector) || a.ServiceAccountName != b.ServiceAccountName || a.RestartPolicy != core.RestartPolicyNever ||
		a.HostNetwork || a.HostPID || a.HostIPC || len(a.InitContainers) != 0 || len(a.EphemeralContainers) != 0 || len(a.Containers) != 1 || len(a.Volumes) != len(b.Volumes) ||
		!reflect.DeepEqual(a.SecurityContext, b.SecurityContext) || !reflect.DeepEqual(a.AutomountServiceAccountToken, b.AutomountServiceAccountToken) {
		return false
	}
	x, y := a.Containers[0], b.Containers[0]
	if x.Name != y.Name || x.Image != y.Image || x.ImagePullPolicy != y.ImagePullPolicy || len(x.Command) != 0 || !reflect.DeepEqual(x.Args, y.Args) ||
		len(x.Env) != 0 || len(x.EnvFrom) != 0 || len(x.Ports) != 0 || x.Lifecycle != nil || !reflect.DeepEqual(x.SecurityContext, y.SecurityContext) ||
		!reflect.DeepEqual(x.VolumeMounts, y.VolumeMounts) || !reflect.DeepEqual(x.Resources, y.Resources) {
		return false
	}
	return reflect.DeepEqual(a.Volumes, b.Volumes)
}

func (b *dsgaCreationBinding) provisionDSGARuntime(ctx context.Context, fact dsgaInitializationResponse) error {
	current, vm, err := b.originalCompute(ctx)
	if err != nil {
		return err
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return err
	}
	s := plan.VM.supply()
	if s == nil {
		return nil
	} // 原手工发行计划只读兼容，不能补新配置。
	if fact.validate(current.intent.createPayload, plan, string(vm.UID)) != nil {
		return errDSGACreationIdentity
	}
	clients, _, err := newDSGAKubeClients(current.server.kube)
	if err != nil {
		return err
	}
	node, err := clients.CoreV1().Nodes().Get(ctx, vm.Status.Node, metav1.GetOptions{})
	if err != nil {
		return err
	}
	config, job, err := dsgaQualificationJob(vm, plan, string(node.UID), fact.Observed.Instance.PGBootID)
	if err != nil {
		return err
	}
	cmAPI := clients.CoreV1().ConfigMaps(s.PlatformNamespace)
	configRaw, err := current.supplyFact(ctx, "collector_config", nil)
	if err != nil {
		return err
	}
	var stored dsgaSupplyObject
	if configRaw != nil && json.Unmarshal(configRaw, &stored) != nil {
		return errDSGACreationIdentity
	}
	actual, err := cmAPI.Get(ctx, config.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && configRaw == nil {
		actual = &core.ConfigMap{}
		err = clients.CoreV1().RESTClient().Post().Namespace(s.PlatformNamespace).Resource("configmaps").Body(config).MaxRetries(0).Do(ctx).Into(actual)
	}
	if err != nil {
		return err
	}
	if actual.UID == "" || actual.DeletionTimestamp != nil || actual.Immutable == nil || !*actual.Immutable || !reflect.DeepEqual(actual.Data, config.Data) || len(actual.BinaryData) != 0 ||
		actual.Labels[dsgaSupplyVMLabel] != string(vm.UID) || len(actual.OwnerReferences) != 0 || configRaw != nil && stored.UID != string(actual.UID) {
		return errDSGACreationIdentity
	}
	if _, err = current.supplyFact(ctx, "collector_config", dsgaSupplyObject{actual.Name, string(actual.UID), managedSQLDigest([]byte(actual.Data["collector.json"]))}); err != nil {
		return err
	}
	jobs := clients.BatchV1().Jobs(s.PlatformNamespace)
	jobRaw, err := current.supplyFact(ctx, "qualification_job", nil)
	if err != nil {
		return err
	}
	stored = dsgaSupplyObject{}
	if jobRaw != nil && json.Unmarshal(jobRaw, &stored) != nil {
		return errDSGACreationIdentity
	}
	actualJob, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && jobRaw == nil {
		actualJob = &batch.Job{}
		err = clients.BatchV1().RESTClient().Post().Namespace(s.PlatformNamespace).Resource("jobs").Body(job).MaxRetries(0).Do(ctx).Into(actualJob)
	}
	if err != nil {
		return err
	}
	if !sameDSGAQualificationJob(actualJob, job) || jobRaw != nil && stored.UID != string(actualJob.UID) {
		return errDSGACreationIdentity
	}
	raw, _ := json.Marshal(job.Spec.Template.Spec)
	if _, err = current.supplyFact(ctx, "qualification_job", dsgaSupplyObject{actualJob.Name, string(actualJob.UID), managedSQLDigest(raw)}); err != nil {
		return err
	}
	result, err := waitDSGAQualification(ctx, clients, actualJob, job)
	if err != nil {
		return err
	}
	binding := journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: plan.Incarnation}
	reader, err := budgetclaims.New(clients.CoreV1(), binding)
	if err != nil {
		return err
	}
	qualified, err := reader.Qualification(ctx, result.Source)
	if err != nil {
		return err
	}
	if qualified.Identity.ComputeInstanceID != plan.ComputeInstanceID || qualified.NodeUID != string(node.UID) || qualified.RunnerPodName != vm.Status.PodName {
		return errDSGACreationIdentity
	}
	for _, target := range s.Targets {
		q, e := qualified.Target(target.VMMemoryBytes)
		if e != nil || q.Limits.MemoryMaxBytes != target.NormalMemoryMaxBytes {
			return errDSGACreationIdentity
		}
	}
	if _, err = current.supplyFact(ctx, "qualification_result", result); err != nil {
		return err
	}
	automatic := s.clone().Automatic
	minMem, maxMem := current.intent.MinMem, current.intent.MaxMem
	if plan.ResumeGeneration {
		minMem, maxMem = plan.ResumeMinMem, plan.ResumeMaxMem
	}
	automatic.MinimumVMMemoryBytes = dsgapi.Uint64(minMem) * (1 << 20)
	automatic.MaximumVMMemoryBytes = dsgapi.Uint64(maxMem) * (1 << 20)
	selection := runtimeselection.Selection{ProtocolVersion: 1, Namespace: vm.Namespace, Name: vm.Name, Instance: fact.Observed.Instance, Incarnation: plan.Incarnation, Generation: 1, Qualification: result.Source, Automatic: &automatic}
	return current.publishInitialRuntimeSelection(ctx, clients, selection, plan)
}

func waitDSGAQualification(ctx context.Context, clients kubernetes.Interface, original, wanted *batch.Job) (qualificationissuer.Result, error) {
	var zero qualificationissuer.Result
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	for {
		got, err := clients.BatchV1().Jobs(original.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
		if err != nil {
			return zero, err
		}
		if got.UID != original.UID || !sameDSGAQualificationJob(got, wanted) {
			return zero, errDSGACreationIdentity
		}
		for _, c := range got.Status.Conditions {
			if c.Type == batch.JobFailed && c.Status == core.ConditionTrue {
				return zero, errors.New("original qualification job failed; preserve evidence and any unconfirmed source")
			}
		}
		if got.Status.Succeeded == 1 {
			pods, err := clients.CoreV1().Pods(got.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/controller-uid=" + string(got.UID)})
			if err != nil {
				return zero, err
			}
			if len(pods.Items) != 1 {
				return zero, errDSGACreationIdentity
			}
			pod := pods.Items[0]
			owner := metav1.GetControllerOf(&pod)
			if owner == nil || owner.UID != got.UID || owner.Kind != "Job" || pod.Spec.NodeName != wanted.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] || pod.Status.Phase != core.PodSucceeded ||
				len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].RestartCount != 0 || pod.Status.ContainerStatuses[0].State.Terminated == nil || pod.Status.ContainerStatuses[0].State.Terminated.ExitCode != 0 {
				return zero, errDSGACreationIdentity
			}
			limit := int64(64 << 10)
			raw, err := clients.CoreV1().Pods(got.Namespace).GetLogs(pod.Name, &core.PodLogOptions{Container: "qualifier", LimitBytes: &limit}).DoRaw(ctx)
			if err != nil {
				return zero, err
			}
			var result qualificationissuer.Result
			if dsgapi.UnmarshalObject(raw, &result) != nil || result.Source.Validate() != nil || len(result.ReportSHA256) != 64 || len(result.LinuxArtifactSHA256) != 64 || len(result.ContinuousArtifactSHA256) != 64 {
				return zero, errDSGACreationIdentity
			}
			// 读取日志后原Pod/Job UID再核，丢回复或替代执行不冒成功。
			again, err := clients.CoreV1().Pods(got.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if err != nil || again.UID != pod.UID {
				return zero, errDSGACreationIdentity
			}
			return result, nil
		}
		if err := waitDSGAStartupPoll(ctx); err != nil {
			return zero, err
		}
	}
}

func (b *dsgaCreationBinding) publishInitialRuntimeSelection(ctx context.Context, clients kubernetes.Interface, selected runtimeselection.Selection, plan dsgaCreationPlan) error {
	if selected.Validate() != nil {
		return errDSGACreationPlan
	}
	s := plan.VM.supply()
	raw, _ := json.Marshal(selected)
	wanted := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: runtimeselection.Name(selected.Instance.VMUID), Namespace: s.PlatformNamespace, Labels: map[string]string{runtimeselection.LabelKey: runtimeselection.LabelValue}}, Data: map[string]string{runtimeselection.DataKey: string(raw)}}
	original, err := b.supplyFact(ctx, "runtime_selection", nil)
	if err != nil {
		return err
	}
	var stored dsgaSupplyObject
	if original != nil && json.Unmarshal(original, &stored) != nil {
		return errDSGACreationIdentity
	}
	cm, err := clients.CoreV1().ConfigMaps(s.PlatformNamespace).Get(ctx, wanted.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && original == nil {
		cm = &core.ConfigMap{}
		err = clients.CoreV1().RESTClient().Post().Namespace(s.PlatformNamespace).Resource("configmaps").Body(wanted).MaxRetries(0).Do(ctx).Into(cm)
	}
	if err != nil {
		return err
	}
	got, err := runtimeselection.Decode(cm, journal.Binding{Namespace: selected.Namespace, Name: selected.Name, VMUID: types.UID(selected.Instance.VMUID), Incarnation: selected.Incarnation}, s.PlatformNamespace)
	if err != nil || got.Digest() != selected.Digest() || original != nil && stored.UID != string(cm.UID) {
		return errDSGACreationIdentity
	}
	_, err = b.supplyFact(ctx, "runtime_selection", dsgaSupplyObject{cm.Name, string(cm.UID), selected.Digest()})
	return err
}
