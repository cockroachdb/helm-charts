package migrate

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMigrationReplacementReady(t *testing.T) {
	controller := true
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-2",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "crdb.cockroachlabs.com/v1beta1", Kind: "CrdbNode",
				Name: "cluster-2", Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	t.Run("ready replacement", func(t *testing.T) {
		require.True(t, migrationReplacementReady(ready))
	})
	t.Run("same-name source pod", func(t *testing.T) {
		pod := ready.DeepCopy()
		pod.OwnerReferences[0] = metav1.OwnerReference{
			APIVersion: "apps/v1", Kind: "StatefulSet", Name: "cluster", Controller: &controller,
		}
		require.False(t, migrationReplacementReady(pod))
	})
	t.Run("replacement not ready", func(t *testing.T) {
		pod := ready.DeepCopy()
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		require.False(t, migrationReplacementReady(pod))
	})
	t.Run("terminating replacement", func(t *testing.T) {
		pod := ready.DeepCopy()
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		require.False(t, migrationReplacementReady(pod))
	})
}
