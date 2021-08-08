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

package resource_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/cockroachdb/helm-charts/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/testutils"
)

func TestClean(t *testing.T) {
	ctx := context.TODO()
	scheme := testutils.InitScheme(t)
	namespace := "test-namespace"
	stsName := "cockroachdb"
	ca := "cockroachdb-ca-secret"
	node := "cockroachdb-node-secret"
	client := "cockroachdb-client-secret"
	other := "other"
	caSecret := secretObj(ca, namespace, nil, nil)
	nodeSecret := secretObj(node, namespace, nil, nil)
	clientSecret := secretObj(client, namespace, nil, nil)
	otherSecret := secretObj(other, namespace, nil, nil)
	fakeClient := testutils.NewFakeClient(scheme, caSecret, nodeSecret, clientSecret, otherSecret)

	require.NoError(t, resource.Clean(ctx, fakeClient, namespace, stsName))

	r := resource.NewKubeResource(ctx, fakeClient, namespace, kube.DefaultPersister)

	// these secrets should not exist
	for _, name := range []string{ca, node, client} {
		_, err := resource.LoadTLSSecret(name, r)
		assert.True(t, apierrors.IsNotFound(err))
	}

	// other secret should exist
	_, err := resource.LoadTLSSecret(other, r)
	require.NoError(t, err)

}
