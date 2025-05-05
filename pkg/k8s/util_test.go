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
	"errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	cgtesting "k8s.io/client-go/testing"
)

func TestSanitizeDriverName(t *testing.T) {
	tests := []struct {
		name     string
		driver   string
		expected string
	}{
		{
			name:     "standard driver name",
			driver:   "ebs.csi.aws.com",
			expected: "ebs-csi-aws-com",
		},
		{
			name:     "driver name with special characters",
			driver:   "ebs.csi.aws.com/special@chars",
			expected: "ebs-csi-aws-com-special-chars",
		},
		{
			name:     "driver name with trailing dash",
			driver:   "ebs.csi.aws.com-",
			expected: "ebs-csi-aws-com-X",
		},
		{
			name:     "driver name with multiple special characters",
			driver:   "ebs.csi.aws.com/special@chars!#$%^&*()",
			expected: "ebs-csi-aws-com-special-chars---------X",
		},
		{
			name:     "driver name with multiple trailing dashes",
			driver:   "ebs.csi.aws.com---",
			expected: "ebs-csi-aws-com---X",
		},
		{
			name:     "empty driver name",
			driver:   "",
			expected: "",
		},
		{
			name:     "driver name with only special characters",
			driver:   "!@#$%^&*()",
			expected: "----------X",
		},
		{
			name:     "eks auto mode driver name",
			driver:   "ebs.csi.eks.amazonaws.com",
			expected: "ebs-csi-eks-amazonaws-com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeDriverName(tt.driver)
			if result != tt.expected {
				t.Errorf("SanitizeDriverName(%q) = %q, expected %q", tt.driver, result, tt.expected)
			}
		})
	}
}

func TestClearMetadata(t *testing.T) {
	testTime := metav1.Time{Time: time.Date(2025, 4, 30, 12, 0, 0, 0, time.UTC)}
	testUID := types.UID("test-uid-12345")
	testResourceVersion := "12345"

	tests := []struct {
		name     string
		metadata metav1.ObjectMeta
		check    func(t *testing.T, metadata metav1.ObjectMeta)
	}{
		{
			name: "clear all fields",
			metadata: metav1.ObjectMeta{
				ResourceVersion:   testResourceVersion,
				UID:               testUID,
				CreationTimestamp: testTime,
				Name:              "test-name",
				Namespace:         "test-namespace",
				Labels: map[string]string{
					"app": "test-app",
				},
				Annotations: map[string]string{
					"annotation": "test-annotation",
				},
			},
			check: func(t *testing.T, metadata metav1.ObjectMeta) {
				// These fields should be cleared
				if metadata.ResourceVersion != "" {
					t.Errorf("ResourceVersion was not cleared, got %q", metadata.ResourceVersion)
				}
				if metadata.UID != "" {
					t.Errorf("UID was not cleared, got %q", metadata.UID)
				}
				if !metadata.CreationTimestamp.IsZero() {
					t.Errorf("CreationTimestamp was not cleared, got %v", metadata.CreationTimestamp)
				}

				// These fields should remain unchanged
				if metadata.Name != "test-name" {
					t.Errorf("Name was modified, expected %q, got %q", "test-name", metadata.Name)
				}
				if metadata.Namespace != "test-namespace" {
					t.Errorf("Namespace was modified, expected %q, got %q", "test-namespace", metadata.Namespace)
				}
				if metadata.Labels["app"] != "test-app" {
					t.Errorf("Labels were modified, expected app=%q, got %q", "test-app", metadata.Labels["app"])
				}
				if metadata.Annotations["annotation"] != "test-annotation" {
					t.Errorf("Annotations were modified, expected annotation=%q, got %q", "test-annotation", metadata.Annotations["annotation"])
				}
			},
		},
		{
			name: "empty metadata",
			metadata: metav1.ObjectMeta{
				Name: "test-name",
			},
			check: func(t *testing.T, metadata metav1.ObjectMeta) {
				if metadata.ResourceVersion != "" {
					t.Errorf("ResourceVersion was not empty, got %q", metadata.ResourceVersion)
				}
				if metadata.UID != "" {
					t.Errorf("UID was not empty, got %q", metadata.UID)
				}
				if !metadata.CreationTimestamp.IsZero() {
					t.Errorf("CreationTimestamp was not zero, got %v", metadata.CreationTimestamp)
				}
				if metadata.Name != "test-name" {
					t.Errorf("Name was modified, expected %q, got %q", "test-name", metadata.Name)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := tt.metadata.DeepCopy()
			ClearMetadata(metadata)
			tt.check(t, *metadata)
		})
	}
}

func TestDryRunRbac(t *testing.T) {
	tests := []struct {
		name      string
		verb      string
		namespace string
		resource  string
		resName   string
		allowed   bool
		wantErr   bool
	}{
		{
			name:      "allowed access",
			verb:      "create",
			namespace: "default",
			resource:  "persistentvolumeclaims",
			resName:   "test-pvc",
			allowed:   true,
			wantErr:   false,
		},
		{
			name:      "denied access",
			verb:      "delete",
			namespace: "default",
			resource:  "persistentvolumes",
			resName:   "test-pv",
			allowed:   false,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake client that will return our desired response
			client := fake.NewClientset()

			// Override the fake client's behavior for SelfSubjectAccessReviews
			client.PrependReactor("create", "selfsubjectaccessreviews", func(action cgtesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.SelfSubjectAccessReview{
					Status: authv1.SubjectAccessReviewStatus{
						Allowed: tt.allowed,
					},
				}, nil
			})

			err := DryRunRbac(context.Background(), client, tt.verb, tt.namespace, tt.resource, tt.resName)

			if tt.wantErr && err == nil {
				t.Errorf("DryRunRbac() expected an error but got nil")
			} else if !tt.wantErr && err != nil {
				t.Errorf("DryRunRbac() expected no error but got: %v", err)
			}
		})
	}
}

func TestWaitForNotFound(t *testing.T) {
	tests := []struct {
		name           string
		callbackFunc   func() func() error
		contextTimeout time.Duration
		wantErr        bool
	}{
		{
			name: "resource found then not found",
			callbackFunc: func() func() error {
				callCount := 0
				return func() error {
					callCount++
					if callCount < 3 {
						// Return a non-NotFound error for the first two calls
						return errors.New("resource still exists")
					}
					// Return a NotFound error on the third call
					return kerrors.NewNotFound(schema.GroupResource{Group: "test", Resource: "test"}, "test-resource")
				}
			},
			contextTimeout: 5 * time.Second,
			wantErr:        false,
		},
		{
			name: "context canceled",
			callbackFunc: func() func() error {
				return func() error {
					// Always return a non-NotFound error
					return errors.New("resource still exists")
				}
			},
			contextTimeout: 100 * time.Millisecond, // Short timeout to trigger context cancellation
			wantErr:        true,
		},
		{
			name: "immediate not found",
			callbackFunc: func() func() error {
				return func() error {
					// Return a NotFound error immediately
					return kerrors.NewNotFound(schema.GroupResource{Group: "test", Resource: "test"}, "test-resource")
				}
			},
			contextTimeout: 5 * time.Second,
			wantErr:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tt.contextTimeout)
			defer cancel()

			err := WaitForNotFound(ctx, "name of object", tt.callbackFunc())

			if tt.wantErr && err == nil {
				t.Errorf("WaitForNotFound() expected an error but got nil")
			} else if !tt.wantErr && err != nil {
				t.Errorf("WaitForNotFound() expected no error but got: %v", err)
			}
		})
	}
}
