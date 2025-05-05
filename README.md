# eks-auto-mode-ebs-migration-tool

The `eks-auto-mode-ebs-migration-tool` is used to migrate a Persistent Volume Claim from a standard EBS CSI StorageClass
(`ebs.csi.aws.com`) to the EKS Auto EBS CSI StorageClass (`ebs.csi.eks.amazonaws.com`) or vice-versa.

**The migration process requires deleting the existing PersistentVolumeClaim/PersistentVolume and re-creating them with the
new StorageClass.**

If running in a cluster with mixed compute (Auto Mode & Non Auto Mode Nodes), ensure that you have appropriate an appropriate
NodeSelector or required Node Affinity to cause Pods with Auto Mode migrated volumes to only schedule against Auto Mode nodes.

```bash
% ./eks-auto-mode-ebs-migration-tool --help
The eks-auto-mode-ebs-migration-tool is used to migrate a Persistent Volume Claim from a
standard EBS CSI StorageClass (ebs.csi.aws.com) to the EKS Auto EBS CSI StorageClass
(ebs.csi.eks.amazonaws.com) or vice versa. To do this, it must delete the PVC/PV that
are currently in use and replace them with new copies updated to use the new StorageClass.
Workloads using the volume must be scaled down or terminated before use, as the EBS Volume
must be detached prior to migration.

Usage of ./eks-auto-mode-ebs-migration-tool:
  -attribution
    	Show attribution
  -cluster-name string
    	Name of the cluster
  -dry-run
    	Run in dry-run mode where validations are performed, but no mutations occur (default true)
  -kubeconfig string
    	Absolute path to the kubeconfig file (default "~/.kube/config")
  -namespace string
    	Namespace for the PVC (default "default")
  -pvc-name string
    	Name of the PVC
  -snapshot
    	Create a snapshot of the EBS volume prior to making any changes (default true)
  -storageclass string
    	New storage class to migrate to
  -yes
    	Override the prompt to accept migration if all validations have passed
```
## Pre-Requisites

The migration process validates and requires that:

- The EBS Volume that backs the PersistentVolume must be unattached from any EC2 Instance. This means scaling down the StatefulSet or Deployment to allow the volume to detach. 
- The new StorageClass must have a `volumeBindingMode` of `WaitForFirstConsumer` to prevent the immediate creation of a PersistentVolume
- The existing PersistentVolume must have a reclaim policy of `Retain` to ensure that the EBS Volume remains when the PersistentVolume is deleted
- The caller needs appropriate Kubernetes permissions to Create/Delete the PersistentVolumeClaim and PersistentVolume.
- The caller needs appropriate AWS IAM permissions to call `DescribeVolume`, and `CreateTags` on the EBS Volume as well as optionally `CreateSnapshot`,

# Sample Usage

First, run the tool in dry-run mode (the default) to perform all validation checks:

```bash
% ./eks-auto-mode-ebs-migration-tool --cluster-name my-cluster-name --pvc-name my-pvc-name -storageclass new-storage-class
2025/03/31 10:06:36 running in dry-run mode
2025/03/31 10:06:38 Found PVC default/my-pvc-name 
2025/03/31 10:06:38 Identified migration from StorageClass existing-storage-class-> new-storage-class
2025/03/31 10:06:40 Found EBS volume vol-12345678901234
2025/03/31 10:06:40 Dry-run completed successfully
```

If this is successful, you can add the `--dry-run=false` argument to the command line to perform the actual migration:

```bash
% ./eks-auto-mode-ebs-migration-tool --cluster-name my-cluster-name --pvc-name my-pvc-name -storageclass new-storage-class --dry-run=false
2025/03/31 10:07:48 running in mutate mode
2025/03/31 10:07:51 Found PVC default/my-pvc-name
2025/03/31 10:07:51 Identified migration from StorageClass existing-storage-class-> new-storage-class
2025/03/31 10:07:52 Found EBS volume vol-12345678901234
2025/03/31 10:07:52 Creating EBS volume snapshot, this can take a few minutes
2025/03/31 10:07:53 snapshot status = pending, waiting for completion
2025/03/31 10:08:03 snapshot status = pending, waiting for completion
2025/03/31 10:08:13 snapshot status = pending, waiting for completion
2025/03/31 10:08:23 snapshot status = pending, waiting for completion
2025/03/31 10:08:33 Created snapshot of vol-12345678901234, snap-12345678901234
Validations were successful. The following operations can fail and will require manual intervention to repair in that case. Type YES to continue with migration
YES
2025/03/31 10:08:53 Existing PVC has been deleted
2025/03/31 10:08:54 Existing PV has been deleted
2025/03/31 10:08:54 Created new PVC my-pvc-name with UID ea5acb08-5a1a-4ffc-ad08-d6ddc24f271b
2025/03/31 10:08:54 Tagging EBS volume to match new PVC
2025/03/31 10:08:54 Created new persistent volume, pvc-ea5acb08-5a1a-4ffc-ad08-d6ddc24f271b
2025/03/31 10:08:54 Migration complete!
```


# Security Information 
When you build solutions on Amazon Web Services, security responsibilities are shared between you and Amazon Cloud. This [Shared Responsibility Model](https://aws.amazon.com/compliance/shared-responsibility-model/) reduces your operational burden due to the Amazon Web Services operations, management, and control components, including host operations The physical security of the system, the virtualization layer, and the facility where the service runs. For more information on Amazon Web Services, visit Amazon Web Services [Cloud Security](http://aws.amazon.com/security/).
