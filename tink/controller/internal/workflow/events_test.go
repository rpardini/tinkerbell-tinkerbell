package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1alpha1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcileEvents(t *testing.T) {
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	tests := map[string]struct {
		status v1alpha1.WorkflowStatus
		want   []string
	}{
		"post actions done succeeds the workflow": {
			status: v1alpha1.WorkflowStatus{
				State:        v1alpha1.WorkflowStatePost,
				CurrentState: &v1alpha1.CurrentState{State: v1alpha1.WorkflowStateSuccess},
			},
			want: []string{"Normal WorkflowSucceeded Workflow completed successfully"},
		},
		"global timeout times out the workflow": {
			status: v1alpha1.WorkflowStatus{
				State:               v1alpha1.WorkflowStateRunning,
				GlobalTimeout:       60,
				GlobalExecutionStop: &past,
			},
			want: []string{"Warning WorkflowTimedOut Workflow timed out: global timeout of 60s reached"},
		},
		"running workflow records nothing": {
			status: v1alpha1.WorkflowStatus{State: v1alpha1.WorkflowStateRunning},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			wf := &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
				Status:     tt.status,
			}
			recorder := events.NewFakeRecorder(10)
			r := NewReconciler(GetFakeClientBuilder().WithRuntimeObjects(wf).Build(), nil, WithEventRecorder(recorder))

			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "wf", Namespace: "default"}}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			close(recorder.Events)

			var got []string
			for e := range recorder.Events {
				got = append(got, e)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("unexpected events (-want +got):\n%s", diff)
			}
		})
	}
}
