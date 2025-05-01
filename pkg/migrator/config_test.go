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
	"strings"
	"testing"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		wantErr     bool
		errContains string
	}{
		{
			name: "valid config",
			config: Config{
				NewStorageClassName: "ebs-sc",
				Namespace:           "default",
				PVCName:             "test-pvc",
				ClusterName:         "test-cluster",
			},
			wantErr: false,
		},
		{
			name: "missing PVC name",
			config: Config{
				NewStorageClassName: "ebs-sc",
				Namespace:           "default",
				PVCName:             "",
				ClusterName:         "test-cluster",
			},
			wantErr:     true,
			errContains: "PVC name must be specified",
		},
		{
			name: "missing storage class name",
			config: Config{
				NewStorageClassName: "",
				Namespace:           "default",
				PVCName:             "test-pvc",
				ClusterName:         "test-cluster",
			},
			wantErr:     true,
			errContains: "new storage class name must be specified",
		},
		{
			name: "missing cluster name",
			config: Config{
				NewStorageClassName: "ebs-sc",
				Namespace:           "default",
				PVCName:             "test-pvc",
				ClusterName:         "",
			},
			wantErr:     true,
			errContains: "cluster name must be specified",
		},
		{
			name: "namespace can be empty",
			config: Config{
				NewStorageClassName: "ebs-sc",
				Namespace:           "",
				PVCName:             "test-pvc",
				ClusterName:         "test-cluster",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Config.Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("Config.Validate() error = %v, should contain %v", err, tt.errContains)
			}
		})
	}
}
