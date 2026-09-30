package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1alpha1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// capturingRecorder is an events.EventRecorder that keeps each Event as
// "<regarding name> <related name> <type> <reason> <note>", "-" standing in for a nil object.
type capturingRecorder struct{ events []string }

func (c *capturingRecorder) Eventf(regarding, related runtime.Object, eventtype, reason, _, note string, args ...any) {
	name := func(o runtime.Object) string {
		if m, ok := o.(metav1.Object); ok && o != nil {
			return m.GetName()
		}
		return "-"
	}
	c.events = append(c.events, fmt.Sprintf("%s %s %s %s %s", name(regarding), name(related), eventtype, reason, fmt.Sprintf(note, args...)))
}

func TestReconcileEvents(t *testing.T) {
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	tests := map[string]struct {
		status v1alpha1.WorkflowStatus
		noHW   bool
		want   []string
	}{
		"post actions done succeeds the workflow": {
			status: v1alpha1.WorkflowStatus{
				State:        v1alpha1.WorkflowStatePost,
				CurrentState: &v1alpha1.CurrentState{State: v1alpha1.WorkflowStateSuccess},
			},
			want: []string{"hw1 wf Normal WorkflowSucceeded Workflow wf: Workflow completed successfully"},
		},
		"global timeout times out the workflow": {
			status: v1alpha1.WorkflowStatus{
				State:               v1alpha1.WorkflowStateRunning,
				GlobalTimeout:       60,
				GlobalExecutionStop: &past,
			},
			want: []string{"hw1 wf Warning WorkflowTimedOut Workflow wf: Workflow timed out: global timeout of 60s reached"},
		},
		"without Hardware it is recorded on the Workflow": {
			status: v1alpha1.WorkflowStatus{
				State:        v1alpha1.WorkflowStatePost,
				CurrentState: &v1alpha1.CurrentState{State: v1alpha1.WorkflowStateSuccess},
			},
			noHW: true,
			want: []string{"wf - Normal WorkflowSucceeded Workflow completed successfully"},
		},
		"running workflow records nothing": {
			status: v1alpha1.WorkflowStatus{State: v1alpha1.WorkflowStateRunning},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			wf := &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
				Spec:       v1alpha1.WorkflowSpec{HardwareRef: "hw1"},
				Status:     tt.status,
			}
			objs := []runtime.Object{wf}
			if !tt.noHW {
				objs = append(objs, &v1alpha1.Hardware{ObjectMeta: metav1.ObjectMeta{Name: "hw1", Namespace: "default"}})
			}
			recorder := &capturingRecorder{}
			r := NewReconciler(GetFakeClientBuilder().WithRuntimeObjects(objs...).Build(), nil, WithEventRecorder(recorder))

			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "wf", Namespace: "default"}}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tt.want, recorder.events); diff != "" {
				t.Fatalf("unexpected events (-want +got):\n%s", diff)
			}
		})
	}
}
