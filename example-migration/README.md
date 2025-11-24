# Example Migration

This directory contains example resources to demonstrate EBS CSI to EKS Auto Mode EBS CSI migration.

## Quick Start

1. Set your cluster name:
   ```bash
   export CLUSTER_NAME=your-cluster-name
   ```

2. Create StorageClasses:
   ```bash
   make setup
   ```

3. Apply PVC and Pod:
   ```bash
   make apply-pod
   ```

4. Delete Pod (required before migration):
   ```bash
   make delete-pod
   ```

5. Migrate forward (ebs-sc → auto-ebs-sc):
   ```bash
   make migrate-forward
   ```

6. Re-apply Pod to test:
   ```bash
   make apply-pod
   ```

7. Delete Pod and migrate backward (auto-ebs-sc → ebs-sc):
   ```bash
   make delete-pod
   make migrate-backward
   ```

## Workflow

```
setup → apply-pod → delete-pod → migrate-forward → apply-pod → delete-pod → migrate-backward → ...
```

You can repeat the migration cycle as many times as needed.

## Commands

- `make help` - Show all available commands
- `make status` - Check current state of resources
- `make clean` - Remove Pod and PVC
