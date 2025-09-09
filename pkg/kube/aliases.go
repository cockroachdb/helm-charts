package kube

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var IsNotFound = apierrors.IsNotFound
