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
package k8s

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	storagev1 "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SanitizeDriverName sanitizes provided driver name, from https://github.com/kubernetes-csi/external-attacher/blob/master/pkg/controller/util.go#L128
func SanitizeDriverName(driver string) string {
	re := regexp.MustCompile("[^a-zA-Z0-9-]")
	name := re.ReplaceAllString(driver, "-")
	if len(name) > 0 && name[len(name)-1] == '-' {
		// name must not end with '-' or be empty
		name = name + "X"
	}
	return name
}

// ClearMetadata clears out the common metadata that we need to erase before recreating the object
func ClearMetadata(m *metav1.ObjectMeta) {
	m.ResourceVersion = ""
	m.UID = ""
	m.CreationTimestamp = metav1.Time{}
}

// GetAndValidateStorageClass retrieves a storage class by name and validates that it has the correct volume binding mode
func GetAndValidateStorageClass(ctx context.Context, cs kubernetes.Interface, storageClassName string) (*storagev1.StorageClass, error) {
	sc, err := cs.StorageV1().StorageClasses().Get(ctx, storageClassName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to find storage class %s, %w", storageClassName, err)
	}
	if sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		return nil, fmt.Errorf("PVC volume binding mode must be set to %s", storagev1.VolumeBindingWaitForFirstConsumer)
	}
	return sc, nil
}

// DryRunRbac creates a self subject access review for the given verb and resource, returning an error if the access
// review fails for any reason
func DryRunRbac(ctx context.Context, cs kubernetes.Interface, verb string, namespace string, resource string, name string) error {
	ar := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Verb:      verb,
				Namespace: namespace,
				Resource:  resource,
				Name:      name,
			},
		},
	}
	rsp, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ar, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to dry-run RBAC %s %s, %w", verb, resource, err)
	}
	if !rsp.Status.Allowed {
		return fmt.Errorf("not authorized to %s %s", verb, resource)
	}
	return nil
}

// WaitForNotFound calls f repeatedly, returning nil when it returns a not found message.
// It will wait 1 second between attempts and will return an error if the context is canceled.
func WaitForNotFound(ctx context.Context, name string, f func() error) error {
	for {
		err := f()
		if err != nil && kerrors.IsNotFound(err) {
			return nil
		}
		log.Printf("waiting for deletion of %s", name)
		timer := time.NewTimer(1 * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			return fmt.Errorf("context canceled while waiting on deletion, %w", ctx.Err())
		}
	}
}
