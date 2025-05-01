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
	"fmt"
)

// Config holds the configuration for the migration process
type Config struct {
	NewStorageClassName string
	Namespace           string
	PVCName             string
	ClusterName         string
}

// Validate checks that the configuration is valid
func (c Config) Validate() error {
	if c.PVCName == "" {
		return fmt.Errorf("PVC name must be specified")
	}
	if c.NewStorageClassName == "" {
		return fmt.Errorf("new storage class name must be specified")
	}
	if c.ClusterName == "" {
		return fmt.Errorf("cluster name must be specified")
	}
	return nil
}
