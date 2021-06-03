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
	"context"

	"github.com/cockroachdb/helm-charts/pkg/kube"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Builder populates a given Kubernetes resource or creates its default instance (placeholder)
type Builder interface {
	Build(client.Object) error
	Placeholder() client.Object
}

// Fetcher updates the object with its state from Kubernetes
type Fetcher interface {
	Fetch(obj client.Object) error
}

// Persister creates or updates the object in Kubernetes after calling the mutation function.
type Persister interface {
	Persist(obj client.Object, mutateFn func() error) (upserted bool, err error)
}

func NewKubeResource(ctx context.Context, client client.Client, namespace string, persistFn kube.PersistFn) Resource {
	return Resource{
		Fetcher:   NewKubeFetcher(ctx, namespace, client),
		Persister: NewKubePersister(ctx, namespace, client, persistFn),
	}
}

// Resource represents a resource that can be fetched or saved
type Resource struct {
	Fetcher
	Persister
}

func NewKubeFetcher(ctx context.Context, namespace string, reader client.Reader) *KubeFetcher {
	return &KubeFetcher{
		ctx:       ctx,
		namespace: namespace,
		Reader:    reader,
	}
}

// KubeFetcher fetches Kubernetes results
type KubeFetcher struct {
	ctx       context.Context
	namespace string
	client.Reader
}

func (f KubeFetcher) Fetch(o client.Object) error {
	accessor, err := meta.Accessor(o)
	if err != nil {
		return err
	}

	err = f.Reader.Get(f.ctx, f.makeKey(accessor.GetName()), o)

	return err
}

func (f KubeFetcher) makeKey(name string) types.NamespacedName {
	return types.NamespacedName{
		Name:      name,
		Namespace: f.namespace,
	}
}

func NewKubePersister(ctx context.Context, namespace string, client client.Client, persistFn kube.PersistFn) *KubePersister {
	return &KubePersister{
		ctx:       ctx,
		namespace: namespace,
		persistFn: persistFn,
		Client:    client,
	}
}

// KubePersister saves resources back into Kubernetes
type KubePersister struct {
	ctx       context.Context
	namespace string
	persistFn kube.PersistFn
	client.Client
}

func (p KubePersister) Persist(obj client.Object, mutateFn func() error) (upserted bool, err error) {
	if err := addNamespace(obj, p.namespace); err != nil {
		return false, err
	}

	return p.persistFn(p.ctx, p.Client, obj, mutateFn)
}

func addNamespace(o runtime.Object, ns string) error {
	accessor, err := meta.Accessor(o)
	if err != nil {
		return errors.Wrapf(err, "failed to access object's meta information")
	}

	accessor.SetNamespace(ns)

	return nil
}
