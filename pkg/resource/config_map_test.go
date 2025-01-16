package resource_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cockroachdb/helm-charts/pkg/kube"
	"github.com/cockroachdb/helm-charts/pkg/resource"
	"github.com/cockroachdb/helm-charts/pkg/testutils"
)

func TestUpdateConfigMap(t *testing.T) {
	scheme := testutils.InitScheme(t)
	fakeClient := testutils.NewFakeClient(scheme)
	namespace := "default"
	name := "test-configmap"

	r := resource.NewKubeResource(context.TODO(), fakeClient, namespace, kube.DefaultPersister)
	cm := resource.CreateConfigMap(namespace, name, []byte{}, r)

	err := cm.Update()
	require.NoError(t, err)

	// fetch the configmap
	cm, err = resource.LoadConfigMap(cm.Name(), r)
	require.NoError(t, err)

	require.Equal(t, "test-configmap-crt", cm.GetConfigMap().Name)
}
