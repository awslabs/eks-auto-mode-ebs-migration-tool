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
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/smithy-go"
	authv1 "k8s.io/api/authorization/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	cgtesting "k8s.io/client-go/testing"
	"strings"
	"testing"
)

// mockEC2Client is a mock implementation of the EC2 client for testing
type mockEC2Client struct {
	DescribeVolumesFunc   func(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	CreateTagsFunc        func(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	CreateSnapshotFunc    func(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error)
	DescribeSnapshotsFunc func(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
}

func (m *mockEC2Client) DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return m.DescribeVolumesFunc(ctx, params, optFns...)
}

func (m *mockEC2Client) CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	return m.CreateTagsFunc(ctx, params, optFns...)
}

func (m *mockEC2Client) CreateSnapshot(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
	return m.CreateSnapshotFunc(ctx, params, optFns...)
}

func (m *mockEC2Client) DescribeSnapshots(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	return m.DescribeSnapshotsFunc(ctx, params, optFns...)
}

// mockEKSClient is a mock implementation of the EKS client for testing
type mockEKSClient struct {
	DescribeClusterFunc func(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}

func (m *mockEKSClient) DescribeCluster(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return m.DescribeClusterFunc(ctx, params, optFns...)
}

func TestMigratorHappyPath(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")
	newPVCUID := types.UID("new-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"
	newPVName := "pvc-new-pvc-uid"
	snapshotID := "snap-12345678901234567"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create storage classes
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	newSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
			Annotations: map[string]string{
				"pv.kubernetes.io/bind-completed":               "yes",
				"pv.kubernetes.io/bound-by-controller":          "yes",
				"volume.kubernetes.io/selected-node":            "node-1",
				"volume.beta.kubernetes.io/storage-class":       oldStorageClassName,
				"volume.beta.kubernetes.io/storage-provisioner": oldStorageClassName,
				"volume.kubernetes.io/storage-provisioner":      oldStorageClassName,
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
		},
	}

	// Create PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by":         oldStorageClassName,
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
			Finalizers: []string{"kubernetes.io/pv-protection", "external-attacher/ebs-csi-aws-com"},
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
			ClaimRef: &v1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				Namespace:       testNamespace,
				Name:            testPVCName,
				UID:             oldPVCUID,
				ResourceVersion: "1",
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: volumeID,
					FSType:       "ext4",
				},
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, oldSC, newSC)

	// Track created/deleted resources
	var createdPVC *v1.PersistentVolumeClaim
	var createdPV *v1.PersistentVolume
	var deletedPVC, deletedPV bool

	// allow everything
	kubeClient.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})
	// Setup reactor for PVC creation
	kubeClient.PrependReactor("create", "persistentvolumeclaims", func(action cgtesting.Action) (bool, runtime.Object, error) {
		createAction := action.(cgtesting.CreateAction)
		obj := createAction.GetObject().(*v1.PersistentVolumeClaim)
		obj.UID = newPVCUID
		obj.ResourceVersion = "2"
		createdPVC = obj
		return false, nil, nil
	})

	// Setup reactor for PV creation
	kubeClient.PrependReactor("create", "persistentvolumes", func(action cgtesting.Action) (bool, runtime.Object, error) {
		createAction := action.(cgtesting.CreateAction)
		obj := createAction.GetObject().(*v1.PersistentVolume)
		obj.ResourceVersion = "2"
		createdPV = obj
		return false, nil, nil
	})

	// Setup reactor for PVC deletion
	kubeClient.PrependReactor("delete", "persistentvolumeclaims", func(action cgtesting.Action) (bool, runtime.Object, error) {
		deletedPVC = true
		return false, nil, nil
	})

	// Setup reactor for PV deletion
	kubeClient.PrependReactor("delete", "persistentvolumes", func(action cgtesting.Action) (bool, runtime.Object, error) {
		deletedPV = true
		return false, nil, nil
	})

	// Setup reactor for get after delete to simulate resource being gone
	kubeClient.PrependReactor("get", "persistentvolumeclaims", func(action cgtesting.Action) (bool, runtime.Object, error) {
		if deletedPVC {
			return true, nil, kerrors.NewNotFound(schema.GroupResource{Group: "", Resource: "persistentvolumeclaims"}, testPVCName)
		}
		return false, nil, nil
	})

	kubeClient.PrependReactor("get", "persistentvolumes", func(action cgtesting.Action) (bool, runtime.Object, error) {
		if deletedPV {
			return true, nil, kerrors.NewNotFound(schema.GroupResource{Group: "", Resource: "persistentvolumes"}, oldPVName)
		}
		return false, nil, nil
	})

	// Create mock EC2 client
	mockEC2 := &mockEC2Client{
		DescribeVolumesFunc: func(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
			return &ec2.DescribeVolumesOutput{
				Volumes: []ec2types.Volume{
					{
						VolumeId: aws.String(volumeID),
						Tags: []ec2types.Tag{
							{
								Key:   aws.String("KubernetesCluster"),
								Value: aws.String(clusterName),
							},
							{
								Key:   aws.String("kubernetes.io/created-for/pvc/name"),
								Value: aws.String(testPVCName),
							},
							{
								Key:   aws.String("kubernetes.io/created-for/pvc/uid"),
								Value: aws.String(string(oldPVCUID)),
							},
						},
						Attachments: []ec2types.VolumeAttachment{}, // No attachments
					},
				},
			}, nil
		},
		CreateTagsFunc: func(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			if params.DryRun != nil && *params.DryRun {
				// For dry run, return DryRunOperation error
				return nil, &smithy.GenericAPIError{
					Code:    "DryRunOperation",
					Message: "Request would have succeeded, but DryRun flag is set",
				}
			}
			return &ec2.CreateTagsOutput{}, nil
		},
		CreateSnapshotFunc: func(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
			return &ec2.CreateSnapshotOutput{
				SnapshotId: aws.String(snapshotID),
			}, nil
		},
		DescribeSnapshotsFunc: func(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
			return &ec2.DescribeSnapshotsOutput{
				Snapshots: []ec2types.Snapshot{
					{
						SnapshotId: aws.String(snapshotID),
						State:      ec2types.SnapshotStateCompleted,
					},
				},
			}, nil
		},
	}

	// Create mock EKS client
	mockEKS := &mockEKSClient{
		DescribeClusterFunc: func(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
			return &eks.DescribeClusterOutput{}, nil
		},
	}

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
		ec2: mockEC2,
		eks: mockEKS,
	}

	// Test the happy path
	ctx := context.Background()

	// Step 1: Validate preconditions
	err := m.ValidatePreconditions(ctx)
	if err != nil {
		t.Fatalf("ValidatePreconditions failed: %v", err)
	}

	// Verify that the migrator has been properly initialized
	if m.originalPVC == nil {
		t.Fatal("originalPVC should be set after validation")
	}
	if m.originalPV == nil {
		t.Fatal("originalPV should be set after validation")
	}
	if m.oldStorageClassName != oldStorageClassName {
		t.Fatalf("oldStorageClassName should be %s, got %s", oldStorageClassName, m.oldStorageClassName)
	}
	if m.newStorageClassProvisioner != "ebs.csi.eks.amazonaws.com" {
		t.Fatalf("newStorageClassProvisioner should be ebs.csi.eks.amazonaws.com, got %s", m.newStorageClassProvisioner)
	}

	// Step 2: Create snapshot
	err = m.PerformSnapshot(ctx)
	if err != nil {
		t.Fatalf("PerformSnapshot failed: %v", err)
	}

	// Step 3: Execute migration
	err = m.Execute(ctx)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Verify that resources were deleted and recreated
	if !deletedPVC {
		t.Error("PVC was not deleted")
	}
	if !deletedPV {
		t.Error("PV was not deleted")
	}

	// Verify the new PVC
	if createdPVC == nil {
		t.Fatal("New PVC was not created")
	}
	if createdPVC.Name != testPVCName {
		t.Errorf("Expected new PVC name to be %s, got %s", testPVCName, createdPVC.Name)
	}
	if *createdPVC.Spec.StorageClassName != newStorageClassName {
		t.Errorf("Expected new PVC storage class to be %s, got %s", newStorageClassName, *createdPVC.Spec.StorageClassName)
	}
	for k, v := range createdPVC.Annotations {
		if v == oldStorageClassName {
			t.Errorf("Expected translation of PVC annotation %s: %s to %s", k, v, newStorageClassName)
		}
	}
	if _, exists := createdPVC.Annotations["pv.kubernetes.io/bind-completed"]; exists {
		t.Error("New PVC should not have bind-completed annotation")
	}

	// Verify the new PV
	if createdPV == nil {
		t.Fatal("New PV was not created")
	}
	if createdPV.Name != newPVName {
		t.Errorf("Expected new PV name to be %s, got %s", newPVName, createdPV.Name)
	}
	if createdPV.Spec.StorageClassName != newStorageClassName {
		t.Errorf("Expected new PV storage class to be %s, got %s", newStorageClassName, createdPV.Spec.StorageClassName)
	}
	if createdPV.Spec.CSI.Driver != "ebs.csi.eks.amazonaws.com" {
		t.Errorf("Expected new PV CSI driver to be ebs.csi.eks.amazonaws.com, got %s", createdPV.Spec.CSI.Driver)
	}
	if createdPV.Spec.CSI.VolumeHandle != volumeID {
		t.Errorf("Expected new PV volume handle to be %s, got %s", volumeID, createdPV.Spec.CSI.VolumeHandle)
	}
	for k, v := range createdPV.Annotations {
		if v == oldStorageClassName {
			t.Errorf("Expected translation of PV annotation %s: %s to %s", k, v, newStorageClassName)
		}
	}

	// Verify finalizers were updated
	foundUpdatedFinalizer := false
	for _, finalizer := range createdPV.Finalizers {
		if finalizer == "external-attacher/ebs-csi-eks-amazonaws-com" {
			foundUpdatedFinalizer = true
			break
		}
	}
	if !foundUpdatedFinalizer {
		t.Error("New PV should have updated finalizer for the new CSI driver")
	}
}

// TestMigratorNoRbac tests that validation fails if we don't have RBAC access for creating/deleting PVCS/PVs
func TestMigratorNoRbac(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create storage classes
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	newSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
			Annotations: map[string]string{
				"pv.kubernetes.io/bind-completed":         "yes",
				"pv.kubernetes.io/bound-by-controller":    "yes",
				"volume.kubernetes.io/selected-node":      "node-1",
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
		},
	}

	// Create PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by":         oldStorageClassName,
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
			Finalizers: []string{"kubernetes.io/pv-protection", "external-attacher/ebs-csi-aws-com"},
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
			ClaimRef: &v1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				Namespace:       testNamespace,
				Name:            testPVCName,
				UID:             oldPVCUID,
				ResourceVersion: "1",
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: volumeID,
					FSType:       "ext4",
				},
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, oldSC, newSC)

	// deny all K8s RBAC
	kubeClient.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: false,
			},
		}, nil
	})

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
	}

	ctx := context.Background()

	err := m.ValidatePreconditions(ctx)
	if err == nil {
		t.Fatalf("expected error due to RBAC failure, got none")
	}
}

func TestMigratorNoTagging(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"
	snapshotID := "snap-12345678901234567"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create storage classes
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	newSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
			Annotations: map[string]string{
				"pv.kubernetes.io/bind-completed":         "yes",
				"pv.kubernetes.io/bound-by-controller":    "yes",
				"volume.kubernetes.io/selected-node":      "node-1",
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
		},
	}

	// Create PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by":         oldStorageClassName,
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
			Finalizers: []string{"kubernetes.io/pv-protection", "external-attacher/ebs-csi-aws-com"},
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
			ClaimRef: &v1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				Namespace:       testNamespace,
				Name:            testPVCName,
				UID:             oldPVCUID,
				ResourceVersion: "1",
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: volumeID,
					FSType:       "ext4",
				},
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, oldSC, newSC)

	// allow everything
	kubeClient.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	// Create mock EC2 client
	mockEC2 := &mockEC2Client{
		DescribeVolumesFunc: func(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
			return &ec2.DescribeVolumesOutput{
				Volumes: []ec2types.Volume{
					{
						VolumeId: aws.String(volumeID),
						Tags: []ec2types.Tag{
							{
								Key:   aws.String("KubernetesCluster"),
								Value: aws.String(clusterName),
							},
							{
								Key:   aws.String("kubernetes.io/created-for/pvc/name"),
								Value: aws.String(testPVCName),
							},
							{
								Key:   aws.String("kubernetes.io/created-for/pvc/uid"),
								Value: aws.String(string(oldPVCUID)),
							},
						},
						Attachments: []ec2types.VolumeAttachment{}, // No attachments
					},
				},
			}, nil
		},
		CreateTagsFunc: func(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			return nil, errors.New("not authorized")
		},
		CreateSnapshotFunc: func(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
			return &ec2.CreateSnapshotOutput{
				SnapshotId: aws.String(snapshotID),
			}, nil
		},
		DescribeSnapshotsFunc: func(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
			return &ec2.DescribeSnapshotsOutput{
				Snapshots: []ec2types.Snapshot{
					{
						SnapshotId: aws.String(snapshotID),
						State:      ec2types.SnapshotStateCompleted,
					},
				},
			}, nil
		},
	}

	// Create mock EKS client
	mockEKS := &mockEKSClient{
		DescribeClusterFunc: func(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
			return &eks.DescribeClusterOutput{}, nil
		},
	}

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
		ec2: mockEC2,
		eks: mockEKS,
	}

	// Test the happy path
	ctx := context.Background()

	// Step 1: Validate preconditions
	err := m.ValidatePreconditions(ctx)
	if err == nil {
		t.Fatalf("Expected validation to fail do to inability to tag instances")
	}
}

func TestMapTags(t *testing.T) {
	tests := []struct {
		name         string
		existingTags []ec2types.Tag
		oldPVCId     string
		newPVCId     string
		expected     []ec2types.Tag
	}{
		{
			name: "replace pvc id in tags",
			existingTags: []ec2types.Tag{
				{
					Key:   aws.String("kubernetes.io/created-for/pvc/uid"),
					Value: aws.String("pvc-old-id"),
				},
				{
					Key:   aws.String("kubernetes.io/created-for/pvc/name"),
					Value: aws.String("test-pvc"),
				},
			},
			oldPVCId: "pvc-old-id",
			newPVCId: "pvc-new-id",
			expected: []ec2types.Tag{
				{
					Key:   aws.String("kubernetes.io/created-for/pvc/uid"),
					Value: aws.String("pvc-new-id"),
				},
				{
					Key:   aws.String("kubernetes.io/created-for/pvc/name"),
					Value: aws.String("test-pvc"),
				},
			},
		},
		{
			name: "replace pvc id in multiple tags",
			existingTags: []ec2types.Tag{
				{
					Key:   aws.String("kubernetes.io/created-for/pvc/uid"),
					Value: aws.String("pvc-old-id"),
				},
				{
					Key:   aws.String("kubernetes.io/cluster/test-cluster"),
					Value: aws.String("owned-by-pvc-old-id"),
				},
			},
			oldPVCId: "pvc-old-id",
			newPVCId: "pvc-new-id",
			expected: []ec2types.Tag{
				{
					Key:   aws.String("kubernetes.io/created-for/pvc/uid"),
					Value: aws.String("pvc-new-id"),
				},
				{
					Key:   aws.String("kubernetes.io/cluster/test-cluster"),
					Value: aws.String("owned-by-pvc-new-id"),
				},
			},
		},
		{
			name:         "empty tags",
			existingTags: []ec2types.Tag{},
			oldPVCId:     "pvc-old-id",
			newPVCId:     "pvc-new-id",
			expected:     []ec2types.Tag{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapTags(tt.existingTags, tt.oldPVCId, tt.newPVCId)

			if len(result) != len(tt.expected) {
				t.Fatalf("Expected %d tags, got %d", len(tt.expected), len(result))
			}

			for i, tag := range result {
				if aws.ToString(tag.Key) != aws.ToString(tt.expected[i].Key) {
					t.Errorf("Tag %d key mismatch: expected %s, got %s", i, aws.ToString(tt.expected[i].Key), aws.ToString(tag.Key))
				}
				if aws.ToString(tag.Value) != aws.ToString(tt.expected[i].Value) {
					t.Errorf("Tag %d value mismatch: expected %s, got %s", i, aws.ToString(tt.expected[i].Value), aws.ToString(tag.Value))
				}
			}
		})
	}
}

func TestValidateEKS(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		eksError    error
		wantErr     bool
	}{
		{
			name:        "valid cluster",
			clusterName: "test-cluster",
			eksError:    nil,
			wantErr:     false,
		},
		{
			name:        "cluster not found",
			clusterName: "non-existent-cluster",
			eksError:    errors.New("cluster not found"),
			wantErr:     true,
		},
		{
			name:        "empty cluster name",
			clusterName: "",
			eksError:    errors.New("invalid cluster name"),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockEKS := &mockEKSClient{
				DescribeClusterFunc: func(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
					if params.Name == nil || *params.Name == "" {
						return nil, errors.New("invalid cluster name")
					}
					if *params.Name != "test-cluster" {
						return nil, errors.New("cluster not found")
					}
					return &eks.DescribeClusterOutput{}, nil
				},
			}

			m := &Migrator{
				cfg: Config{
					ClusterName: tt.clusterName,
				},
				eks: mockEKS,
			}

			err := m.validateEKS(context.Background())

			if tt.wantErr && err == nil {
				t.Error("validateEKS() expected error but got nil")
			} else if !tt.wantErr && err != nil {
				t.Errorf("validateEKS() unexpected error: %v", err)
			}
		})
	}
}
func TestValidateK8sWithInvalidStorageClass(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer
	immediateBinding := storagev1.VolumeBindingImmediate

	// Create storage classes - new storage class has immediate binding which should fail validation
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	newSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &immediateBinding, // This should cause validation to fail
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
			Annotations: map[string]string{
				"pv.kubernetes.io/bind-completed":         "yes",
				"pv.kubernetes.io/bound-by-controller":    "yes",
				"volume.kubernetes.io/selected-node":      "node-1",
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
		},
	}

	// Create PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by":         oldStorageClassName,
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
			Finalizers: []string{"kubernetes.io/pv-protection", "external-attacher/ebs-csi-aws-com"},
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
			ClaimRef: &v1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				Namespace:       testNamespace,
				Name:            testPVCName,
				UID:             oldPVCUID,
				ResourceVersion: "1",
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: volumeID,
					FSType:       "ext4",
				},
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, oldSC, newSC)

	// allow everything
	kubeClient.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
	}

	// Test validation with invalid storage class
	ctx := context.Background()
	err := m.validateK8s(ctx)

	// Should fail because new storage class has immediate binding
	if err == nil {
		t.Fatal("validateK8s() should fail with invalid storage class binding mode")
	}

	if !strings.Contains(err.Error(), "volume binding mode") {
		t.Errorf("Expected error about volume binding mode, got: %v", err)
	}
}
func TestValidateK8sWithInvalidPVReclaimPolicy(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create storage classes
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	newSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
			Annotations: map[string]string{
				"pv.kubernetes.io/bind-completed":         "yes",
				"pv.kubernetes.io/bound-by-controller":    "yes",
				"volume.kubernetes.io/selected-node":      "node-1",
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
		},
	}

	// Create PV with Delete reclaim policy (should fail validation)
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by":         oldStorageClassName,
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
			Finalizers: []string{"kubernetes.io/pv-protection", "external-attacher/ebs-csi-aws-com"},
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimDelete, // This should cause validation to fail
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
			ClaimRef: &v1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				Namespace:       testNamespace,
				Name:            testPVCName,
				UID:             oldPVCUID,
				ResourceVersion: "1",
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: volumeID,
					FSType:       "ext4",
				},
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, oldSC, newSC)

	// allow everything
	kubeClient.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
	}

	// Test validation with invalid PV reclaim policy
	ctx := context.Background()
	err := m.validateK8s(ctx)

	// Should fail because PV has Delete reclaim policy instead of Retain
	if err == nil {
		t.Fatal("validateK8s() should fail with invalid PV reclaim policy")
	}

	if !strings.Contains(err.Error(), "reclaim policy") {
		t.Errorf("Expected error about reclaim policy, got: %v", err)
	}
}
func TestValidateEC2WithAttachedVolume(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	instanceID := "i-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")

	// Create mock EC2 client that returns a volume with attachments
	mockEC2 := &mockEC2Client{
		DescribeVolumesFunc: func(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
			return &ec2.DescribeVolumesOutput{
				Volumes: []ec2types.Volume{
					{
						VolumeId: aws.String(volumeID),
						Tags: []ec2types.Tag{
							{
								Key:   aws.String("KubernetesCluster"),
								Value: aws.String(clusterName),
							},
						},
						Attachments: []ec2types.VolumeAttachment{
							{
								InstanceId: aws.String(instanceID),
								State:      ec2types.VolumeAttachmentStateAttached,
							},
						}, // Volume is attached to an instance
					},
				},
			}, nil
		},
	}

	// Create migrator with mocks
	m := &Migrator{
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
		ec2: mockEC2,
		originalPV: &v1.PersistentVolume{
			Spec: v1.PersistentVolumeSpec{
				PersistentVolumeSource: v1.PersistentVolumeSource{
					CSI: &v1.CSIPersistentVolumeSource{
						VolumeHandle: volumeID,
					},
				},
			},
		},
		originalPVC: &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				UID: oldPVCUID,
			},
		},
	}

	// Test validation with attached volume
	ctx := context.Background()
	err := m.validateEC2(ctx)

	// Should fail because volume is still attached
	if err == nil {
		t.Fatal("validateEC2() should fail with attached volume")
	}

	if !strings.Contains(err.Error(), "attached") {
		t.Errorf("Expected error about attached volume, got: %v", err)
	}
}
func TestValidateEC2WithClusterNameMismatch(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")

	// Create mock EC2 client that returns a volume with mismatched cluster tag
	mockEC2 := &mockEC2Client{
		DescribeVolumesFunc: func(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
			return &ec2.DescribeVolumesOutput{
				Volumes: []ec2types.Volume{
					{
						VolumeId: aws.String(volumeID),
						Tags: []ec2types.Tag{
							{
								Key:   aws.String("KubernetesCluster"),
								Value: aws.String("different-cluster"), // Mismatched cluster name
							},
						},
						Attachments: []ec2types.VolumeAttachment{}, // No attachments
					},
				},
			}, nil
		},
	}

	// Create migrator with mocks
	m := &Migrator{
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
		ec2: mockEC2,
		originalPV: &v1.PersistentVolume{
			Spec: v1.PersistentVolumeSpec{
				PersistentVolumeSource: v1.PersistentVolumeSource{
					CSI: &v1.CSIPersistentVolumeSource{
						VolumeHandle: volumeID,
					},
				},
			},
		},
		originalPVC: &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				UID: oldPVCUID,
			},
		},
	}

	// Test validation with mismatched cluster name
	ctx := context.Background()
	err := m.validateEC2(ctx)

	// Should fail because volume has a different cluster name tag
	if err == nil {
		t.Fatal("validateEC2() should fail with mismatched cluster name")
	}

	if !strings.Contains(err.Error(), "cluster name") {
		t.Errorf("Expected error about cluster name mismatch, got: %v", err)
	}
}
func TestValidateK8sWithNonCSIPV(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create storage classes
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	newSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
			Annotations: map[string]string{
				"pv.kubernetes.io/bind-completed":         "yes",
				"pv.kubernetes.io/bound-by-controller":    "yes",
				"volume.kubernetes.io/selected-node":      "node-1",
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
		},
	}

	// Create PV with non-CSI volume source (should fail validation)
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by":         oldStorageClassName,
				"volume.beta.kubernetes.io/storage-class": oldStorageClassName,
			},
			Finalizers: []string{"kubernetes.io/pv-protection"},
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
			ClaimRef: &v1.ObjectReference{
				Kind:            "PersistentVolumeClaim",
				Namespace:       testNamespace,
				Name:            testPVCName,
				UID:             oldPVCUID,
				ResourceVersion: "1",
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				// Using AWS EBS but not CSI
				AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
					VolumeID: volumeID,
					FSType:   "ext4",
				},
				// No CSI field
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, oldSC, newSC)

	// allow everything
	kubeClient.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
	}

	// Test validation with non-CSI PV
	ctx := context.Background()
	err := m.validateK8s(ctx)

	// Should fail because PV is not managed by CSI
	if err == nil {
		t.Fatal("validateK8s() should fail with non-CSI PV")
	}

	if !strings.Contains(err.Error(), "not managed by CSI") {
		t.Errorf("Expected error about PV not managed by CSI, got: %v", err)
	}
}
func TestExecuteWithFailedPVCDeletion(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
		},
	}

	// Create PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: volumeID,
				},
			},
		},
	}

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create fake Kubernetes client with error on PVC deletion
	kubeClient := fake.NewClientset(pvc, pv, sc, oldSC)
	// all dry-runs pass
	kubeClient.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})
	kubeClient.PrependReactor("delete", "persistentvolumeclaims", func(action cgtesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated PVC deletion error")
	})

	mockEC2 := &mockEC2Client{
		DescribeVolumesFunc: func(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
			return &ec2.DescribeVolumesOutput{
				Volumes: []ec2types.Volume{
					{
						VolumeId: aws.String(volumeID),
						Tags: []ec2types.Tag{
							{
								Key:   aws.String("KubernetesCluster"),
								Value: aws.String(clusterName),
							},
							{
								Key:   aws.String("kubernetes.io/created-for/pvc/name"),
								Value: aws.String(testPVCName),
							},
							{
								Key:   aws.String("kubernetes.io/created-for/pvc/uid"),
								Value: aws.String(string(oldPVCUID)),
							},
						},
						Attachments: []ec2types.VolumeAttachment{}, // No attachments
					},
				},
			}, nil
		},
		CreateTagsFunc: func(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			if params.DryRun != nil && *params.DryRun {
				// For dry run, return DryRunOperation error
				return nil, &smithy.GenericAPIError{
					Code:    "DryRunOperation",
					Message: "Request would have succeeded, but DryRun flag is set",
				}
			}
			return &ec2.CreateTagsOutput{}, nil
		},
		CreateSnapshotFunc: func(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
			return &ec2.CreateSnapshotOutput{
				SnapshotId: aws.String("123"),
			}, nil
		},
		DescribeSnapshotsFunc: func(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
			return &ec2.DescribeSnapshotsOutput{
				Snapshots: []ec2types.Snapshot{
					{
						SnapshotId: aws.String("123"),
						State:      ec2types.SnapshotStateCompleted,
					},
				},
			}, nil
		},
	}
	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
		eks: &mockEKSClient{
			DescribeClusterFunc: func(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
				return &eks.DescribeClusterOutput{}, nil
			},
		},
		ec2:         mockEC2,
		originalPVC: pvc.DeepCopy(),
		originalPV:  pv.DeepCopy(),
	}

	// Test execution with PVC deletion failure
	ctx := context.Background()
	err := m.ValidatePreconditions(ctx)
	if err != nil {
		t.Fatalf("Validate() should succeed, got %s", err)
	}
	err = m.Execute(ctx)

	// Should fail because PVC deletion failed
	if err == nil {
		t.Fatal("Execute() should fail when PVC deletion fails")
	}

	if !strings.Contains(err.Error(), "unable to delete existing PVC") {
		t.Errorf("Expected error about PVC deletion, got: %v", err)
	}
}
func TestPerformSnapshotWithError(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"

	// Create mock EC2 client that fails to create snapshot
	mockEC2 := &mockEC2Client{
		CreateSnapshotFunc: func(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
			return nil, errors.New("simulated snapshot creation error")
		},
	}

	// Create migrator with mocks
	m := &Migrator{
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
		ec2: mockEC2,
		volume: ec2types.Volume{
			VolumeId: aws.String(volumeID),
		},
	}

	// Test snapshot creation with error
	ctx := context.Background()
	err := m.PerformSnapshot(ctx)

	// Should fail because snapshot creation failed
	if err == nil {
		t.Fatal("PerformSnapshot() should fail when snapshot creation fails")
	}

	if !strings.Contains(err.Error(), "unable to create snapshot") {
		t.Errorf("Expected error about snapshot creation, got: %v", err)
	}
}

func TestPerformSnapshotWithPendingSnapshot(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"
	volumeID := "vol-12345678901234567"
	snapshotID := "snap-12345678901234567"

	// Create mock EC2 client with snapshot that transitions from pending to completed
	callCount := 0
	mockEC2 := &mockEC2Client{
		CreateSnapshotFunc: func(ctx context.Context, params *ec2.CreateSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
			return &ec2.CreateSnapshotOutput{
				SnapshotId: aws.String(snapshotID),
			}, nil
		},
		DescribeSnapshotsFunc: func(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
			callCount++
			state := ec2types.SnapshotStatePending
			if callCount > 2 {
				state = ec2types.SnapshotStateCompleted
			}
			return &ec2.DescribeSnapshotsOutput{
				Snapshots: []ec2types.Snapshot{
					{
						SnapshotId: aws.String(snapshotID),
						State:      state,
					},
				},
			}, nil
		},
	}

	// Create migrator with mocks
	m := &Migrator{
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
		ec2: mockEC2,
		volume: ec2types.Volume{
			VolumeId: aws.String(volumeID),
		},
	}

	// Test snapshot creation with pending state
	ctx := context.Background()
	err := m.PerformSnapshot(ctx)

	// Should succeed after snapshot transitions to completed
	if err != nil {
		t.Fatalf("PerformSnapshot() unexpected error: %v", err)
	}

	// Should have called DescribeSnapshots at least 3 times
	if callCount < 3 {
		t.Errorf("Expected at least 3 calls to DescribeSnapshots, got %d", callCount)
	}
}
func TestValidateK8sWithSameStorageClass(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	storageClassName := "ebs-sc"
	clusterName := "test-cluster"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create storage class
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: storageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			VolumeName:       oldPVName,
		},
	}

	// Create PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              storageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: "vol-12345678901234567",
				},
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, sc)

	// Create migrator with mocks - trying to migrate to the same storage class
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: storageClassName, // Same as current
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
	}

	// Test validation with same storage class
	ctx := context.Background()
	err := m.validateK8s(ctx)

	// Should fail because we're trying to migrate to the same storage class
	if err == nil {
		t.Fatal("validateK8s() should fail when migrating to the same storage class")
	}

	if !strings.Contains(err.Error(), "already using storage class") {
		t.Errorf("Expected error about already using storage class, got: %v", err)
	}
}
func TestValidateK8sWithMissingPVC(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	newStorageClassName := "ebs-auto-sc"
	clusterName := "test-cluster"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create storage class
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: newStorageClassName,
		},
		Provisioner:       "ebs.csi.eks.amazonaws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create fake Kubernetes client with no PVC
	kubeClient := fake.NewClientset(sc)

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName,
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
	}

	// Test validation with missing PVC
	ctx := context.Background()
	err := m.validateK8s(ctx)

	// Should fail because PVC doesn't exist
	if err == nil {
		t.Fatal("validateK8s() should fail when PVC doesn't exist")
	}

	if !strings.Contains(err.Error(), "getting PVC") {
		t.Errorf("Expected error about getting PVC, got: %v", err)
	}
}

func TestValidateK8sWithMissingStorageClass(t *testing.T) {
	// Setup test data
	testNamespace := "default"
	testPVCName := "test-pvc"
	oldStorageClassName := "ebs-sc"
	newStorageClassName := "ebs-auto-sc" // This storage class doesn't exist
	clusterName := "test-cluster"
	oldPVCUID := types.UID("old-pvc-uid")
	oldPVName := "pvc-old-pvc-uid"

	// Create volume binding mode for storage classes
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer

	// Create only the old storage class
	oldSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldStorageClassName,
		},
		Provisioner:       "ebs.csi.aws.com",
		VolumeBindingMode: &waitForFirstConsumer,
	}

	// Create PVC
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPVCName,
			Namespace: testNamespace,
			UID:       oldPVCUID,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &oldStorageClassName,
			VolumeName:       oldPVName,
		},
	}

	// Create PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldPVName,
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              oldStorageClassName,
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "ebs.csi.aws.com",
					VolumeHandle: "vol-12345678901234567",
				},
			},
		},
	}

	// Create fake Kubernetes client
	kubeClient := fake.NewClientset(pvc, pv, oldSC)

	// Create migrator with mocks
	m := &Migrator{
		kubeClient: kubeClient,
		cfg: Config{
			NewStorageClassName: newStorageClassName, // This doesn't exist
			Namespace:           testNamespace,
			PVCName:             testPVCName,
			ClusterName:         clusterName,
		},
	}

	// Test validation with missing storage class
	ctx := context.Background()
	err := m.validateK8s(ctx)

	// Should fail because new storage class doesn't exist
	if err == nil {
		t.Fatal("validateK8s() should fail when new storage class doesn't exist")
	}

	if !strings.Contains(err.Error(), "unable to find storage class") {
		t.Errorf("Expected error about finding storage class, got: %v", err)
	}
}
