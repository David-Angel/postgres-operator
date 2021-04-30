package postgrescluster

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"go.opentelemetry.io/otel/oteltest"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

func TestReconcilerRolloutInstance(t *testing.T) {
	ctx := context.Background()

	t.Run("Singleton", func(t *testing.T) {
		instances := []*Instance{
			{
				Name: "one",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1",
						Name:      "one-pod-bruh",
						Labels: map[string]string{
							"controller-revision-hash":               "gamma",
							"postgres-operator.crunchydata.com/role": "master",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		key := client.ObjectKey{Namespace: "ns1", Name: "one-pod-bruh"}
		reconciler := &Reconciler{}
		reconciler.Client = fake.NewClientBuilder().WithObjects(instances[0].Pods[0]).Build()

		assert.NilError(t, reconciler.Client.Get(ctx, key, &corev1.Pod{}),
			"bug in test: expected pod to exist")

		assert.NilError(t, reconciler.rolloutInstance(ctx, observed, instances[0]))

		err := reconciler.Client.Get(ctx, key, &corev1.Pod{})
		assert.Assert(t, apierrors.IsNotFound(err),
			"expected pod to be deleted, got: %#v", err)
	})

	t.Run("Multiple", func(t *testing.T) {
		instances := []*Instance{
			{
				Name: "primary",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1",
						Name:      "the-pod",
						Labels: map[string]string{
							"controller-revision-hash":               "gamma",
							"postgres-operator.crunchydata.com/role": "master",
						},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
			{
				Name:   "other",
				Pods:   []*corev1.Pod{{}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		t.Run("Success", func(t *testing.T) {
			execCalls := 0
			reconciler := &Reconciler{}
			reconciler.PodExec = func(
				namespace, pod, container string, _ io.Reader, stdout, _ io.Writer, command ...string,
			) error {
				execCalls++

				// Execute on the Pod.
				assert.Equal(t, namespace, "ns1")
				assert.Equal(t, pod, "the-pod")
				assert.Equal(t, container, "database")

				// A switchover to any viable candidate.
				assert.DeepEqual(t, command[:2], []string{"patronictl", "switchover"})
				assert.Assert(t, sets.NewString(command...).Has("--master=the-pod"))
				assert.Assert(t, sets.NewString(command...).Has("--candidate="))

				// Indicate success through stdout.
				_, _ = stdout.Write([]byte("switched over"))

				return nil
			}

			assert.NilError(t, reconciler.rolloutInstance(ctx, observed, instances[0]))
			assert.Equal(t, execCalls, 1, "expected PodExec to be called")
		})

		t.Run("Failure", func(t *testing.T) {
			reconciler := &Reconciler{}
			reconciler.PodExec = func(
				_, _, _ string, _ io.Reader, _, _ io.Writer, _ ...string,
			) error {
				// Nothing useful in stdout.
				return nil
			}

			err := reconciler.rolloutInstance(ctx, observed, instances[0])
			assert.ErrorContains(t, err, "switchover")
		})
	})
}

func TestReconcilerRolloutInstances(t *testing.T) {
	ctx := context.Background()
	reconciler := &Reconciler{Tracer: oteltest.DefaultTracer()}

	accumulate := func(on *[]*Instance) func(context.Context, *Instance) error {
		return func(_ context.Context, i *Instance) error { *on = append(*on, i); return nil }
	}

	logSpanAttributes := func(t testing.TB) {
		recorder := new(oteltest.StandardSpanRecorder)
		provider := oteltest.NewTracerProvider(oteltest.WithSpanRecorder(recorder))

		former := reconciler.Tracer
		reconciler.Tracer = provider.Tracer("")

		t.Cleanup(func() {
			reconciler.Tracer = former
			for _, span := range recorder.Completed() {
				b, _ := json.Marshal(span.Attributes())
				t.Log(span.Name(), string(b))
			}
		})
	}

	// Nothing specified, nothing observed, nothing to do.
	t.Run("Empty", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		observed := new(observedInstances)

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed,
			func(context.Context, *Instance) error {
				t.Fatal("expected no redeploys")
				return nil
			}))
	})

	// Single healthy instance; nothing to do.
	t.Run("Steady", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{
			{Name: "00", Replicas: initialize.Int32(1)},
		}
		instances := []*Instance{
			{
				Name: "one",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash":               "gamma",
							"postgres-operator.crunchydata.com/role": "master",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
		}
		observed := &observedInstances{forCluster: instances}

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed,
			func(context.Context, *Instance) error {
				t.Fatal("expected no redeploys")
				return nil
			}))
	})

	// Single healthy instance, Pod does not match PodTemplate.
	t.Run("SingletonOutdated", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{
			{Name: "00", Replicas: initialize.Int32(1)},
		}
		instances := []*Instance{
			{
				Name: "one",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash":               "beta",
							"postgres-operator.crunchydata.com/role": "master",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
		}
		observed := &observedInstances{forCluster: instances}

		var redeploys []*Instance

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed, accumulate(&redeploys)))
		assert.Equal(t, len(redeploys), 1)
		assert.Equal(t, redeploys[0].Name, "one")
	})

	// Two ready instances do not match PodTemplate, no primary.
	t.Run("ManyOutdated", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{
			{Name: "00", Replicas: initialize.Int32(2)},
		}
		instances := []*Instance{
			{
				Name: "one",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
			{
				Name: "two",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
		}
		observed := &observedInstances{forCluster: instances}

		var redeploys []*Instance

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed, accumulate(&redeploys)))
		assert.Equal(t, len(redeploys), 1)
		assert.Equal(t, redeploys[0].Name, "one", `expected the "lowest" name`)
	})

	// Two ready instances do not match PodTemplate, with primary. The replica is redeployed.
	t.Run("ManyOutdatedWithPrimary", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{
			{Name: "00", Replicas: initialize.Int32(2)},
		}
		instances := []*Instance{
			{
				Name: "outdated",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash":               "beta",
							"postgres-operator.crunchydata.com/role": "master",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
			{
				Name: "not-primary",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
		}
		observed := &observedInstances{forCluster: instances}

		var redeploys []*Instance

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed, accumulate(&redeploys)))
		assert.Equal(t, len(redeploys), 1)
		assert.Equal(t, redeploys[0].Name, "not-primary")
	})

	// Two instances do not match PodTemplate, one is not ready. Redeploy that one.
	t.Run("ManyOutdatedWithNotReady", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{
			{Name: "00", Replicas: initialize.Int32(2)},
		}
		instances := []*Instance{
			{
				Name: "outdated",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
			{
				Name: "not-ready",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
		}
		observed := &observedInstances{forCluster: instances}

		var redeploys []*Instance

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed, accumulate(&redeploys)))
		assert.Equal(t, len(redeploys), 1)
		assert.Equal(t, redeploys[0].Name, "not-ready")
	})

	// Two instances do not match PodTemplate, one is terminating. Do nothing.
	t.Run("ManyOutdatedWithTerminating", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{
			{Name: "00", Replicas: initialize.Int32(2)},
		}
		instances := []*Instance{
			{
				Name: "outdated",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
			{
				Name: "terminating",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						DeletionTimestamp: new(metav1.Time),
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
		}
		observed := &observedInstances{forCluster: instances}

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed,
			func(context.Context, *Instance) error {
				t.Fatal("expected no redeploys")
				return nil
			}))
	})

	// Two instances do not match PodTemplate, one is orphaned. Do nothing.
	t.Run("ManyOutdatedWithOrphan", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{
			{Name: "00", Replicas: initialize.Int32(2)},
		}
		instances := []*Instance{
			{
				Name: "outdated",
				Spec: &cluster.Spec.InstanceSets[0],
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
			{
				Name: "orphan",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"controller-revision-hash": "beta",
						},
					},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						UpdateRevision:     "gamma",
					},
				},
			},
		}
		observed := &observedInstances{forCluster: instances}

		logSpanAttributes(t)
		assert.NilError(t, reconciler.rolloutInstances(ctx, cluster, observed,
			func(context.Context, *Instance) error {
				t.Fatal("expected no redeploys")
				return nil
			}))
	})
}
