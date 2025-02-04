/*
Copyright 2021 The Cockroach Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resource

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ConfigMap struct {
	Resource

	configMap *corev1.ConfigMap
}

// CreateConfigMap creates a ConfigMap in the specified namespace
func CreateConfigMap(namespace string, secretName string, data []byte, r Resource) *ConfigMap {
	// Define the ConfigMap object
	configMap := &ConfigMap{
		Resource: r,
		configMap: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-crt", secretName),
			},
			Data: map[string]string{
				"ca.crt": string(data),
			},
		},
	}
	return configMap
}

func (c *ConfigMap) Update() error {
	data := c.configMap.Data
	_, err := c.Persist(c.configMap, func() error {
		c.configMap.Data = data
		return nil
	})

	return err
}

func LoadConfigMap(name string, r Resource) (*ConfigMap, error) {
	c := &ConfigMap{
		Resource: r,
		configMap: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}

	if err := r.Fetch(c.configMap); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *ConfigMap) GetConfigMap() *corev1.ConfigMap {
	return c.configMap
}

func (c *ConfigMap) Name() string {
	return c.configMap.Name
}
