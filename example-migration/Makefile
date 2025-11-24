.PHONY: help setup apply-pod delete-pod migrate-forward migrate-backward clean status

CLUSTER_NAME ?= my-cluster
NAMESPACE ?= default
PVC_NAME ?= test-pvc

help:
	@echo "Usage:"
	@echo "  make setup              - Create StorageClasses"
	@echo "  make apply-pod          - Apply PVC and Pod"
	@echo "  make delete-pod         - Delete Pod (required before migration)"
	@echo "  make migrate-forward    - Migrate from ebs-sc to auto-ebs-sc"
	@echo "  make migrate-backward   - Migrate from auto-ebs-sc back to ebs-sc"
	@echo "  make status             - Show PVC and Pod status"
	@echo "  make clean              - Delete all resources"
	@echo ""
	@echo "Environment variables:"
	@echo "  CLUSTER_NAME=$(CLUSTER_NAME)"
	@echo "  NAMESPACE=$(NAMESPACE)"
	@echo "  PVC_NAME=$(PVC_NAME)"

setup:
	kubectl apply -f ebs-sc.yaml
	kubectl apply -f auto-ebs-sc.yaml
	@echo "StorageClasses created"

apply-pod:
	@kubectl get pvc -n $(NAMESPACE) $(PVC_NAME) >/dev/null 2>&1 || kubectl apply -f pvc.yaml
	kubectl apply -f pod.yaml
	@echo "Waiting for pod to be ready..."
	kubectl wait --for=condition=ready pod/test-pod -n $(NAMESPACE) --timeout=120s

delete-pod:
	kubectl delete -f pod.yaml --ignore-not-found=true
	@echo "Waiting for volume to detach..."
	@sleep 10

migrate-forward:
	@echo "Migrating from ebs-sc to auto-ebs-sc..."
	../eks-auto-mode-ebs-migration-tool \
		--cluster-name $(CLUSTER_NAME) \
		--namespace $(NAMESPACE) \
		--pvc-name $(PVC_NAME) \
		--storageclass auto-ebs-sc \
		--dry-run=false \
		--yes

migrate-backward:
	@echo "Migrating from auto-ebs-sc back to ebs-sc..."
	../eks-auto-mode-ebs-migration-tool \
		--cluster-name $(CLUSTER_NAME) \
		--namespace $(NAMESPACE) \
		--pvc-name $(PVC_NAME) \
		--storageclass ebs-sc \
		--dry-run=false \
		--yes

status:
	@echo "=== StorageClasses ==="
	@kubectl get sc ebs-sc auto-ebs-sc 2>/dev/null || echo "StorageClasses not found"
	@echo ""
	@echo "=== PVC ==="
	@kubectl get pvc -n $(NAMESPACE) $(PVC_NAME) 2>/dev/null || echo "PVC not found"
	@echo ""
	@echo "=== Pod ==="
	@kubectl get pod -n $(NAMESPACE) test-pod 2>/dev/null || echo "Pod not found"

clean:
	kubectl delete -f pod.yaml --ignore-not-found=true
	kubectl delete -f pvc.yaml --ignore-not-found=true
	@echo "Cleaned up Pod and PVC"
	@echo "Note: PV may remain due to Retain policy - delete manually if needed"
