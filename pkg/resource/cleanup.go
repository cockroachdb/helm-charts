/*
Copyright 2021 The Cockroach Authors

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

package resource

import (
	"context"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Clean(ctx context.Context, cl client.Client, namespace string, stsName string) {

	secrets := []string{stsName + "-ca-secret", stsName + "-node-secret", stsName + "-client-secret"}

	secret := &corev1.Secret{}

	for i := range secrets {
		secret.SetName(secrets[i])
		secret.SetNamespace(namespace)
		if err := cl.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
			logrus.Errorf("Failed to delete secret %s: error %s", secret.GetName(), err.Error())
			// if error occurs, continue and try to clean as much as possible
			continue
		}
	}
}
