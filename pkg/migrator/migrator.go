/*
Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package migrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/smithy-go"
	"github.com/awslabs/eks-auto-mode-ebs-migration-tool/pkg/k8s"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/csi-translation-lib/plugins"
	"k8s.io/klog/v2"
)

type EC2Client interface {
	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	CreateSnapshot(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error)
	DescribeSnapshots(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
}
type EKSClient interface {
	DescribeCluster(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}
type Migrator struct {
	kubeClient                 kubernetes.Interface
	cfg                        Config
	ec2                        EC2Client
	newStorageClassProvisioner string
	oldStorageClassProvisioner string
	oldVolumeName              string
	eks                        EKSClient
	// originalPVC/PV are the unmodified PVC/PV that we will be replacing
	originalPVC          *v1.PersistentVolumeClaim
	originalPV           *v1.PersistentVolume
	originalStorageClass *storagev1.StorageClass
	oldStorageClassName  string
	newStorageClass      *storagev1.StorageClass
	volume               types.Volume
	tagsToUpdate         []types.Tag
	inTreeTranslator     plugins.InTreePlugin
}

func New(cs kubernetes.Interface, awsCfg aws.Config, cfg Config) *Migrator {

	return &Migrator{
		kubeClient:       cs,
		cfg:              cfg,
		ec2:              ec2.NewFromConfig(awsCfg),
		eks:              eks.NewFromConfig(awsCfg),
		inTreeTranslator: plugins.NewAWSElasticBlockStoreCSITranslator(),
	}
}

func (m *Migrator) ValidatePreconditions(ctx context.Context) error {
	if err := m.validateK8s(ctx); err != nil {
		return err
	}
	if err := m.validateEKS(ctx); err != nil {
		return err
	}
	if err := m.validateEC2(ctx); err != nil {
		return err
	}
	return nil
}

var validSCProvisionerRegexp = regexp.MustCompile(`[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*`)

func (m *Migrator) validateK8s(ctx context.Context) error {
	// First get the storage class without validation to check the provisioner
	newSc, err := m.kubeClient.StorageV1().StorageClasses().Get(ctx, m.cfg.NewStorageClassName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("unable to find storage class %s, %w", m.cfg.NewStorageClassName, err)
	}
	
	// Only validate Auto Mode requirements if migrating to Auto Mode
	validateAutoSC := newSc.Provisioner == k8s.AutoModeEBSProvisioner
	newSc, err = k8s.GetAndValidateStorageClass(ctx, m.kubeClient, m.cfg.NewStorageClassName, validateAutoSC)
	if err != nil {
		return fmt.Errorf("validating new storage class, %s", err)
	}
	m.newStorageClass = newSc
	m.newStorageClassProvisioner = newSc.Provisioner

	pvc, err := m.kubeClient.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(ctx, m.cfg.PVCName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting PVC, %s", err)
	}
	klog.InfoS("Found PVC", "pvc", klog.KObj(pvc))
	if pvc.Spec.StorageClassName == nil {
		return errors.New("PVC has no storage class specified")
	}

	// find and validate the StatefulSet that owns this PVC, if any
	if err := m.validateOwnerStatefulSet(ctx, pvc); err != nil {
		return err
	}

	m.oldStorageClassName = *pvc.Spec.StorageClassName
	if m.oldStorageClassName == m.cfg.NewStorageClassName {
		return fmt.Errorf("PVC is already using storage class of %s", m.cfg.NewStorageClassName)
	}

	oldSc, err := k8s.GetAndValidateStorageClass(ctx, m.kubeClient, m.oldStorageClassName, false)
	if err != nil {
		return fmt.Errorf("unable to get old storage class, %s", err)
	}
	m.originalStorageClass = oldSc
	m.oldStorageClassProvisioner = oldSc.Provisioner

	if pvc.Spec.VolumeName == "" {
		return fmt.Errorf("PVC %s/%s has no volume name specified", pvc.Namespace, pvc.Name)
	}
	// update our volume name since we don't know it yet
	m.oldVolumeName = pvc.Spec.VolumeName
	// validate that we can delete/create PVCs/PVs
	for _, verb := range []string{"delete", "create"} {
		klog.InfoS("Validating ability to perform action on PVC",
			"action", verb,
			"namespace", m.cfg.Namespace,
			"pvcName", m.cfg.PVCName)
		if err := k8s.DryRunRbac(ctx, m.kubeClient, verb, m.cfg.Namespace, "persistentvolumeclaims", m.cfg.PVCName); err != nil {
			return fmt.Errorf("unable to validate ability to %s PVC, %s", verb, err)
		}
		klog.InfoS("Validating ability to perform action on PV",
			"action", verb,
			"namespace", m.cfg.Namespace,
			"pvName", m.oldVolumeName)
		if err := k8s.DryRunRbac(ctx, m.kubeClient, verb, m.cfg.Namespace, "persistentvolumes", m.oldVolumeName); err != nil {
			return fmt.Errorf("unable to validate ability to %s PV, %s", verb, err)
		}
	}

	klog.InfoS("Identified migration from StorageClass to new StorageClass",
		"oldStorageClass", m.oldStorageClassName,
		"newStorageClass", m.cfg.NewStorageClassName)

	pv, err := m.kubeClient.CoreV1().PersistentVolumes().Get(ctx, m.oldVolumeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("unable to find volume named %q referenced from PersistentVolumeClaim", m.oldVolumeName)
	}
	// validate that the volume is created by a CSI
	if pv.Spec.CSI == nil && pv.Spec.AWSElasticBlockStore == nil {
		return fmt.Errorf("PV %s not managed by CSI or legacy in-tree AWS EBS driver", m.oldVolumeName)
	}
	if pv.Spec.CSI != nil && pv.Spec.AWSElasticBlockStore != nil {
		return fmt.Errorf("PV %s has both a CSI and legacy in-tree AWS EBS driver configuration", m.oldVolumeName)
	}
	// and that the EBS volume backing the PV won't be deleted if we delete the volum
	if pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimRetain {
		return fmt.Errorf("persistent volume reclaim policy for PersistentVolume %s be set to retain, currently set to %s",
			m.oldVolumeName, pv.Spec.PersistentVolumeReclaimPolicy)
	}

	m.originalPV = pv.DeepCopy()
	// translate from our in-tree PV & CSI to the out of tree
	if pv.Spec.AWSElasticBlockStore != nil {
		lg := klog.NewKlogr()
		m.originalPV, err = m.inTreeTranslator.TranslateInTreePVToCSI(lg, m.originalPV)
		if err != nil {
			return fmt.Errorf("unable to translate PV %s to in-tree AWS EBS driver", m.oldVolumeName)
		}
		m.originalStorageClass, err = m.inTreeTranslator.TranslateInTreeStorageClassToCSI(lg, m.originalStorageClass)
		if err != nil {
			return fmt.Errorf("unable to translate StorageClass %s to in-tree AWS EBS driver", m.oldStorageClassName)
		}
		m.originalStorageClass.Provisioner = "ebs.csi.aws.com"
	}

	// Validate that we have a CSI driver after potential translation
	if m.originalPV.Spec.CSI == nil {
		return fmt.Errorf("PV %s not managed by CSI after translation", m.oldVolumeName)
	}

	m.originalPVC = pvc.DeepCopy()
	return nil
}

func (m *Migrator) validateEC2(ctx context.Context) (err error) {
	// validate that the volume is backed by an EBS volume we can find
	volumes, err := m.ec2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{m.originalPV.Spec.CSI.VolumeHandle}})
	if err != nil {
		return fmt.Errorf("unable to describe volume %s, %s", m.originalPV.Spec.CSI.VolumeHandle, err)
	}
	if len(volumes.Volumes) != 1 {
		return fmt.Errorf("expected to find one volume, found %d", len(volumes.Volumes))
	}
	m.volume = volumes.Volumes[0]
	klog.InfoS("Found EBS volume", "volumeId", aws.ToString(m.volume.VolumeId))

	if len(m.volume.Attachments) != 0 {
		return fmt.Errorf("can't migrate volume %s that is still attached to an instance", aws.ToString(m.volume.VolumeId))
	}

	oldPVCID := fmt.Sprintf("pvc-%s", m.originalPVC.UID)

	// For Auto Mode, the storage controller policy only has permissions to attach volumes with the
	// eks:eks-cluster-name tag set to the specific cluster, so we need to update the volume's tag
	m.tagsToUpdate = append(m.tagsToUpdate, types.Tag{
		Key:   aws.String("eks:eks-cluster-name"),
		Value: aws.String(m.cfg.ClusterName),
	})

	for _, tag := range m.volume.Tags {
		// any tag that references the old PVC ID needs to be updated to match the new PVC ID
		if strings.Contains(aws.ToString(tag.Value), oldPVCID) {
			m.tagsToUpdate = append(m.tagsToUpdate, tag)
		}

		// look for cases where the volume is tagged for a different cluster than the one we expect it to be
		switch aws.ToString(tag.Key) {
		case "KubernetesCluster", "eks:eks-cluster-name":
			if aws.ToString(tag.Value) != m.cfg.ClusterName {
				return fmt.Errorf("volume %s has s cluster name tag of %s=%s that doesn't match the cluster name specified, %s",
					aws.ToString(m.volume.VolumeId), aws.ToString(tag.Key), aws.ToString(tag.Value), m.cfg.ClusterName)
			}
		}
	}

	klog.InfoS("Validating ability to modify volume tags", "volumeId", aws.ToString(m.volume.VolumeId))
	// make a dry-run call to see if we can create tags on the volume
	_, err = m.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{aws.ToString(m.volume.VolumeId)},
		Tags:      mapTags(m.tagsToUpdate, oldPVCID, "test-value"),
		DryRun:    aws.Bool(true),
	})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) {
			if ae.ErrorCode() != "DryRunOperation" {
				return fmt.Errorf("unable to validate ability to create tags on volume, %s", err)
			}
		} else {
			return fmt.Errorf("unable to validate ability to create tags on volume, %s", err)
		}
	}
	return nil
}

func (m *Migrator) validateEKS(ctx context.Context) error {
	klog.InfoS("Validating cluster exists", "clusterName", m.cfg.ClusterName)
	// validate that the cluster name is real
	c, err := m.eks.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &m.cfg.ClusterName})
	if err != nil {
		return fmt.Errorf("unable to find cluster %s", m.cfg.ClusterName)
	}
	klog.InfoS("Found cluster", "clusterArn", aws.ToString(c.Cluster.Arn))
	return nil
}

func (m *Migrator) PerformSnapshot(ctx context.Context) error {
	klog.InfoS("Creating EBS volume snapshot, this can take a few minutes")
	rsp, err := m.ec2.CreateSnapshot(ctx,
		&ec2.CreateSnapshotInput{
			VolumeId: m.volume.VolumeId,
			Description: aws.String(fmt.Sprintf("Snapshot of %s prior to migration of PVC %s",
				aws.ToString(m.volume.VolumeId), m.cfg.PVCName)),
			TagSpecifications: []types.TagSpecification{
				{
					ResourceType: types.ResourceTypeSnapshot,
					Tags: []types.Tag{
						{
							Key:   aws.String("eks:eks-cluster-name"),
							Value: aws.String(m.cfg.ClusterName),
						},
						{
							Key:   aws.String("KubernetesCluster"),
							Value: aws.String(m.cfg.ClusterName),
						},
						{
							Key:   aws.String("PVCName"),
							Value: aws.String(m.cfg.PVCName),
						},
					},
				},
			},
		})
	if err != nil {
		return fmt.Errorf("unable to create snapshot of volume %s, %s", aws.ToString(m.volume.VolumeId), err)
	}
	if err := waitOnSnapshot(ctx, m.ec2, rsp.SnapshotId); err != nil {
		return fmt.Errorf("snapshot never completed, %s", err)
	}
	klog.InfoS("Created snapshot",
		"volumeId", aws.ToString(m.volume.VolumeId),
		"snapshotId", aws.ToString(rsp.SnapshotId))

	return nil
}
func (m *Migrator) Execute(ctx context.Context) error {
	pvc := m.originalPVC.DeepCopy()
	// clear out the PVC metadata/status so we can recreate it later
	k8s.ClearMetadata(&pvc.ObjectMeta)
	pvc.Status = v1.PersistentVolumeClaimStatus{}
	pvc.Spec.VolumeName = ""
	// change the storage class from the old to the new value everywhere
	pvc.Spec.StorageClassName = &m.cfg.NewStorageClassName
	for k, v := range pvc.Annotations {
		if v == m.oldStorageClassName {
			pvc.Annotations[k] = m.cfg.NewStorageClassName
		}
		if v == m.oldStorageClassProvisioner {
			pvc.Annotations[k] = m.newStorageClassProvisioner
		}
	}
	// and delete the annotations so the controller won't put it in the terminal "Lost" already and will
	// instead be in "Pending" status
	delete(pvc.Annotations, "pv.kubernetes.io/bind-completed")
	delete(pvc.Annotations, "pv.kubernetes.io/bound-by-controller")
	delete(pvc.Annotations, "volume.kubernetes.io/selected-node")

	pv := m.originalPV.DeepCopy()

	// clear out metadata
	k8s.ClearMetadata(&pv.ObjectMeta)
	pv.Status = v1.PersistentVolumeStatus{}

	// Ensure node affinity structure exists
	if pv.Spec.NodeAffinity == nil {
		pv.Spec.NodeAffinity = &v1.VolumeNodeAffinity{Required: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{{}}}}
	} else if pv.Spec.NodeAffinity.Required == nil {
		pv.Spec.NodeAffinity.Required = &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{{}}}
	} else if len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) == 0 {
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms = []v1.NodeSelectorTerm{{}}
	}

	for i := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		// Update legacy topology keys
		for j := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms[i].MatchExpressions {
			if pv.Spec.NodeAffinity.Required.NodeSelectorTerms[i].MatchExpressions[j].Key == "topology.ebs.csi.aws.com/zone" {
				pv.Spec.NodeAffinity.Required.NodeSelectorTerms[i].MatchExpressions[j].Key = "topology.kubernetes.io/zone"
			}
		}
		
		// Remove existing Auto Mode compute-type requirement
		filtered := []v1.NodeSelectorRequirement{}
		for _, expr := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms[i].MatchExpressions {
			if expr.Key != "eks.amazonaws.com/compute-type" {
				filtered = append(filtered, expr)
			}
		}
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms[i].MatchExpressions = filtered
		
		// Add compute-type requirement only if migrating to Auto Mode
		if m.newStorageClassProvisioner == k8s.AutoModeEBSProvisioner {
			pv.Spec.NodeAffinity.Required.NodeSelectorTerms[i].MatchExpressions = append(
				pv.Spec.NodeAffinity.Required.NodeSelectorTerms[i].MatchExpressions,
				v1.NodeSelectorRequirement{
					Key:      "eks.amazonaws.com/compute-type",
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"auto"},
				},
			)
		}
	}

	// change the storage class from the old to the new value everywhere
	pv.Spec.StorageClassName = m.cfg.NewStorageClassName

	// Ensure we have a valid CSI spec after translation
	if pv.Spec.CSI == nil {
		return fmt.Errorf("PV %s does not have a valid CSI spec after translation", pv.Name)
	}

	// Validate volume ID format
	volumeID := aws.ToString(m.volume.VolumeId)
	if !strings.HasPrefix(volumeID, "vol-") {
		return fmt.Errorf("invalid EBS volume ID format: %s", volumeID)
	}

	pv.Spec.CSI.Driver = m.newStorageClassProvisioner // from the new storage class we described
	for k, v := range pv.Annotations {
		if v == m.oldStorageClassName {
			pv.Annotations[k] = m.cfg.NewStorageClassName
		}
		if v == m.oldStorageClassProvisioner {
			pvc.Annotations[k] = m.newStorageClassProvisioner
		}
	}

	// update the finalizer on the volume to refer to the new driver name
	oldSanitizedDriverName := k8s.SanitizeDriverName(m.originalStorageClass.Provisioner)
	newSanitizedDriverName := k8s.SanitizeDriverName(m.newStorageClass.Provisioner)
	for i := 0; i < len(pv.Finalizers); i++ {
		if strings.HasSuffix(pv.Finalizers[i], oldSanitizedDriverName) {
			name := pv.Finalizers[i]
			pv.Finalizers[i] = name[0:len(name)-len(oldSanitizedDriverName)] + newSanitizedDriverName
		}
	}

	// delete our old PVC as we need to create one with the same name
	err := m.kubeClient.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Delete(ctx, m.cfg.PVCName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("unable to delete existing PVC, %s", err)
	}

	// wait for the PVC to be removed
	if err := k8s.WaitForNotFound(ctx, fmt.Sprintf("PVC %s", m.cfg.PVCName), func() error {
		_, err := m.kubeClient.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(ctx, m.cfg.PVCName, metav1.GetOptions{})
		return err
	}); err != nil {
		// intentionally not exiting here as its better to just proceed
		klog.V(1).InfoS("Unable to verify deletion of existing PVC", "error", err)
	}

	err = m.kubeClient.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
	if err != nil {
		// technically if we don't delete the volume, it will hang around but that should be ok and is likely better
		// than aborting after we delete the PVC
		klog.V(1).InfoS("Unable to delete volume, continuing", "error", err)
	}
	// wait for the PV to be removed
	if err := k8s.WaitForNotFound(ctx, fmt.Sprintf("PV %s", pv.Name), func() error {
		_, err := m.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
		return err
	}); err != nil {
		klog.V(1).InfoS("Unable to verify deletion of existing PVC", "error", err)
	}

	// create our new PVC
	newPVC, err := m.kubeClient.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	klog.V(4).InfoS("Created PVC with annotations", "annotations", pvc.Annotations)
	if err != nil {
		return fmt.Errorf("unable to create new PVC, %s", err)
	}
	newPVCID := fmt.Sprintf("pvc-%s", newPVC.UID)
	klog.InfoS("Created new PVC", "pvc", klog.KObj(newPVC), "uid", newPVC.UID)

	// update the tags on our EBS volume to refer to the newly created PVC by its UID
	klog.InfoS("Tagging EBS volume to match new PVC")
	oldPVCID := fmt.Sprintf("pvc-%s", m.originalPVC.UID)
	_, err = m.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{aws.ToString(m.volume.VolumeId)},
		Tags:      mapTags(m.tagsToUpdate, oldPVCID, newPVCID),
	})
	if err != nil {
		return fmt.Errorf("unable to create tags on volume, %s", err)
	}

	// modify our volume to point to the newly created PVC
	pv.Name = newPVCID
	pv.Spec.ClaimRef.Name = newPVC.Name
	pv.Spec.ClaimRef.ResourceVersion = newPVC.ResourceVersion
	pv.Spec.ClaimRef.UID = newPVC.UID

	newPV, err := m.kubeClient.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating new PersistentVolume, %s", err)
	}
	klog.InfoS("Created new persistent volume", "volume", klog.KObj(newPV))
	return nil
}

func mapTags(existingTags []types.Tag, oldPVCId, newPVCId string) []types.Tag {
	var tags []types.Tag
	for i := 0; i < len(existingTags); i++ {
		tags = append(tags, types.Tag{
			Key:   aws.String(aws.ToString(existingTags[i].Key)),
			Value: aws.String(strings.ReplaceAll(aws.ToString(existingTags[i].Value), oldPVCId, newPVCId)),
		})
	}
	return tags
}

// waitOnSnapshot waits indefinitely for the supplied snapshots ID to be complete
func waitOnSnapshot(ctx context.Context, client EC2Client, snapshotID *string) error {
	for {
		rsp, err := client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
			SnapshotIds: []string{aws.ToString(snapshotID)},
		})
		if err != nil {
			return fmt.Errorf("describing snapshot, %w", err)
		}
		if len(rsp.Snapshots) != 1 {
			return fmt.Errorf("snapshot not found")
		}

		currentStatus := rsp.Snapshots[0].State
		// finished if the current status is now complete
		if currentStatus == types.SnapshotStateCompleted {
			return nil
		}
		klog.InfoS("Snapshot status", "status", currentStatus, "message", "waiting for completion")
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			return fmt.Errorf("context canceled while waiting on snapshot completion, %w", ctx.Err())
		}

	}
}

// validateOwnerStatefulSet attempts to find the StatefulSet that owns the PVC
// and validates that its volumeClaimTemplates don't contain a storageClass
func (m *Migrator) validateOwnerStatefulSet(ctx context.Context, pvc *v1.PersistentVolumeClaim) error {
	// StatefulSet PVCs follow the pattern: <statefulset-name>-<ordinal>
	idx := strings.LastIndexByte(pvc.Name, '-')
	if idx == -1 {
		return nil
	}
	stsName := pvc.Name[:idx]
	lastPart := pvc.Name[idx+1:]

	var volumeClaimTemplate *v1.PersistentVolumeClaim
	var owningStatefulSet *appsv1.StatefulSet
	extractTemplate := func(sts *appsv1.StatefulSet) {
		// Check if any of the volumeClaimTemplates would create our PVC
		for _, template := range sts.Spec.VolumeClaimTemplates {
			expectedPvcName1 := fmt.Sprintf("%s-%s", template.Name, lastPart)
			expectedPvcName2 := fmt.Sprintf("%s-%s-%s", template.Name, sts.Name, lastPart)
			if pvc.Name == expectedPvcName1 || pvc.Name == expectedPvcName2 {
				volumeClaimTemplate = template.DeepCopy()
				owningStatefulSet = sts
				break
			}
		}
	}
	// Try to get the StatefulSet directly by name
	sts, err := m.kubeClient.AppsV1().StatefulSets(m.cfg.Namespace).Get(ctx, stsName, metav1.GetOptions{})
	if err == nil {
		extractTemplate(sts)
	}
	if owningStatefulSet == nil {
		// StatefulSet name can have the volume claim name as the prefix, so try with that name as well
		idx = strings.IndexByte(stsName, '-')
		if idx == -1 {
			return nil
		}
		stsName = stsName[idx+1:]
		sts, err = m.kubeClient.AppsV1().StatefulSets(m.cfg.Namespace).Get(ctx, stsName, metav1.GetOptions{})
		if err == nil {
			extractTemplate(sts)
		}
	}

	if owningStatefulSet != nil && volumeClaimTemplate != nil {
		klog.InfoS("Found owner StatefulSet", "statefulSet", klog.KObj(owningStatefulSet), "template", volumeClaimTemplate.Name)
		// Validate that the template doesn't specify a storageClass
		existingSCName := aws.ToString(volumeClaimTemplate.Spec.StorageClassName)
		if existingSCName != "" && existingSCName != m.cfg.NewStorageClassName {
			return fmt.Errorf("StatefulSet %s has different storageClassName specified (%s) in volumeClaimTemplate %s",
				owningStatefulSet.Name, existingSCName, volumeClaimTemplate.Name)
		}
		if existingSCName == "" {
			newStorageClassIsDefault := false
			for k, v := range m.newStorageClass.Annotations {
				if k == "storageclass.kubernetes.io/is-default-class" && v == "true" {
					newStorageClassIsDefault = true
				}
			}
			if !newStorageClassIsDefault {
				return fmt.Errorf("StatefulSet %s has no storageClassName specified in volumeClaimTemplate %s and new storage class isn't the default",
					owningStatefulSet.Name, volumeClaimTemplate.Name)
			}
			klog.InfoS("Validated owning StatefulSet volumeClaimTemplate has no storageClassName specified",
				"statefulSet", klog.KObj(owningStatefulSet),
				"template", volumeClaimTemplate.Name)
		} else {
			klog.InfoS("Validated owning StatefulSet volumeClaimTemplate has correct StorageClass name",
				"statefulSet", klog.KObj(owningStatefulSet),
				"template", volumeClaimTemplate.Name,
				"storageClassName", existingSCName)
		}

	}

	// No matching StatefulSet found, which is fine - not all PVCs are owned by StatefulSets
	klog.V(4).InfoS("No owner StatefulSet found for PVC", "pvcName", pvc.Name)
	return nil
}
