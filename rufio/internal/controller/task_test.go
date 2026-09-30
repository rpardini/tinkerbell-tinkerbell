package controller_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	"github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	"github.com/tinkerbell/tinkerbell/rufio/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func toPtr[T any](v T) *T {
	return &v
}

func getAction(s string) bmc.Action {
	switch s {
	case "PowerOn":
		return bmc.Action{PowerAction: toPtr(bmc.PowerOn)}
	case "HardOff":
		return bmc.Action{PowerAction: toPtr(bmc.PowerHardOff)}
	case "SoftOff":
		return bmc.Action{PowerAction: toPtr(bmc.PowerSoftOff)}
	case "BootPXE":
		return bmc.Action{OneTimeBootDeviceAction: &bmc.OneTimeBootDeviceAction{Devices: []bmc.BootDevice{bmc.PXE}}}
	case "VirtualMedia":
		return bmc.Action{VirtualMediaAction: &bmc.VirtualMediaAction{MediaURL: "http://example.com/image.iso", Kind: bmc.VirtualMediaCD}}
	default:
		return bmc.Action{}
	}
}

func TestTaskReconcile(t *testing.T) {
	tests := map[string]struct {
		taskName   string
		action     bmc.Action
		provider   *testProvider
		secret     *corev1.Secret
		task       *bmc.Task
		shouldErr  bool
		timeoutErr bool
	}{
		"success power on": {
			taskName: "PowerOn",
			action:   getAction("PowerOn"),
			provider: &testProvider{Powerstate: "on", PowerSetOK: true},
		},
		"success hard off": {
			taskName: "HardOff",
			action:   getAction("HardOff"),
			provider: &testProvider{Powerstate: "off", PowerSetOK: true},
		},
		"success soft off": {
			taskName: "SoftOff",
			action:   getAction("SoftOff"),
			provider: &testProvider{Powerstate: "off", PowerSetOK: true},
		},
		"success boot pxe": {
			taskName: "BootPXE",
			action:   getAction("BootPXE"),
			provider: &testProvider{BootdeviceOK: true},
		},
		"success virtual media": {
			taskName: "VirtualMedia",
			action:   getAction("VirtualMedia"),
			provider: &testProvider{VirtualMediaOK: true},
		},
		"success power on with rpc provider": {
			taskName: "PowerOn",
			action:   getAction("PowerOn"),
			provider: &testProvider{Powerstate: "on", PowerSetOK: true, Proto: "rpc"},
			secret:   createHMACSecret(),
			task:     createTaskWithRPC("PowerOn", getAction("PowerOn"), createHMACSecret()),
		},
		"success power on with RPC provider w/o secrets": {
			taskName: "PowerOn",
			action:   getAction("PowerOn"),
			provider: &testProvider{Powerstate: "on", PowerSetOK: true, Proto: "rpc"},
		},
		"failure on bmc open": {
			taskName: "PowerOn", action: getAction("PowerOn"),
			provider:  &testProvider{ErrOpen: errors.New("failed to open")},
			shouldErr: true,
		},
		"failure on bmc power on": {
			taskName:  "PowerOn",
			action:    getAction("PowerOn"),
			provider:  &testProvider{ErrPowerStateSet: errors.New("failed to set power state")},
			shouldErr: true,
		},
		"failure on set boot device": {
			taskName:  "BootPXE",
			action:    getAction("BootPXE"),
			provider:  &testProvider{ErrBootDeviceSet: errors.New("failed to set boot device")},
			shouldErr: true,
		},
		"failure on virtual media": {
			taskName:  "VirtualMedia",
			action:    getAction("VirtualMedia"),
			provider:  &testProvider{ErrVirtualMediaInsert: errors.New("failed to set virtual media")},
			shouldErr: true,
		},
		"failure timeout": {
			taskName:   "PowerOn",
			action:     getAction("PowerOn"),
			provider:   &testProvider{Powerstate: "off", PowerSetOK: true},
			timeoutErr: true,
		},
		"failure to find secret": {
			taskName:  "PowerOn",
			action:    getAction("PowerOn"),
			provider:  &testProvider{Powerstate: "off", PowerSetOK: true},
			secret:    &corev1.Secret{},
			task:      createTask("PowerOn", getAction("PowerOn"), &corev1.Secret{}),
			shouldErr: true,
		},
		"success with boot device": {
			taskName: "boot device pxe",
			action: bmc.Action{
				BootDevice: &bmc.BootDeviceConfig{
					Device:     bmc.PXE,
					Persistent: true,
					EFIBoot:    true,
				},
			},
			provider: &testProvider{BootdeviceOK: true},
		},
		"failure on boot device set": {
			taskName: "boot device pxe",
			action: bmc.Action{
				BootDevice: &bmc.BootDeviceConfig{
					Device:     bmc.PXE,
					Persistent: true,
					EFIBoot:    true,
				},
			},
			provider:  &testProvider{ErrBootDeviceSet: errors.New("failed to set boot device")},
			shouldErr: true,
		},
		"failure to find task": {
			taskName:  "empty task",
			action:    bmc.Action{},
			provider:  &testProvider{},
			shouldErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var secret *corev1.Secret
			if tt.secret != nil {
				secret = tt.secret
			} else {
				secret = createSecret()
			}
			var task *bmc.Task
			if tt.task != nil {
				task = tt.task
			} else {
				task = createTask(tt.taskName, tt.action, secret)
			}

			cluster := newClientBuilder().
				WithObjects(task, secret).
				Build()

			reconciler := controller.NewTaskReconciler(cluster, &events.FakeRecorder{}, newTestClient(tt.provider))
			request := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: task.Namespace,
					Name:      task.Name,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), request)
			if !tt.shouldErr && err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			if tt.shouldErr && err == nil {
				t.Fatalf("expected err, got: %v", err)
			}
			if tt.shouldErr {
				return
			}
			if diff := cmp.Diff(result, ctrl.Result{}); diff != "" {
				t.Fatalf("expected no diff, got: %v", diff)
			}

			var retrieved bmc.Task
			if err = cluster.Get(context.Background(), request.NamespacedName, &retrieved); err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			// TODO: g.Expect(retrieved.Status.StartTime.Unix()).To(gomega.BeNumerically("~", time.Now().Unix(), 2))
			if !retrieved.Status.CompletionTime.IsZero() {
				t.Fatalf("expected completion time to be zero, got: %v", retrieved.Status.CompletionTime)
			}
			if len(retrieved.Status.Conditions) != 0 {
				t.Fatalf("expected no conditions, got: %v", retrieved.Status.Conditions)
			}

			// Timeout check
			if tt.timeoutErr {
				expired := metav1.NewTime(retrieved.Status.StartTime.Add(-time.Hour))
				retrieved.Status.StartTime = &expired
				if err = cluster.Status().Update(context.Background(), &retrieved); err != nil {
					t.Fatalf("expected nil err, got: %v", err)
				}

				result, err = reconciler.Reconcile(context.Background(), request)
				if err == nil {
					t.Fatalf("expected err, got: %v", err)
				}
				if diff := cmp.Diff(result, ctrl.Result{}); diff != "" {
					t.Fatalf("expected no diff, got: %v", diff)
				}
				return
			}

			// Ensure re-reconciling a task does sends it into a success state.
			result, err = reconciler.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			if diff := cmp.Diff(result, reconcile.Result{}); diff != "" {
				t.Fatalf("expected no diff, got: %v", diff)
			}

			err = cluster.Get(context.Background(), request.NamespacedName, &retrieved)
			if err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			// TODO: g.Expect(retrieved.Status.CompletionTime.Unix()).To(gomega.BeNumerically("~", time.Now().Unix(), 2))
			if len(retrieved.Status.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got: %v", retrieved.Status.Conditions)
			}
			if retrieved.Status.Conditions[0].Type != bmc.TaskCompleted {
				t.Fatalf("expected condition type to be %s, got: %s", bmc.TaskCompleted, retrieved.Status.Conditions[0].Type)
			}
			if retrieved.Status.Conditions[0].Status != bmc.ConditionTrue {
				t.Fatalf("expected condition status to be %s, got: %s", bmc.ConditionTrue, retrieved.Status.Conditions[0].Status)
			}

			var retrieved2 bmc.Task
			err = cluster.Get(context.Background(), request.NamespacedName, &retrieved2)
			if err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			if diff := cmp.Diff(retrieved2, retrieved); diff != "" {
				t.Fatalf("expected no diff, got: %v", diff)
			}
		})
	}
}

func createTask(name string, action bmc.Action, secret *corev1.Secret) *bmc.Task {
	return &bmc.Task{
		TypeMeta: metav1.TypeMeta{
			APIVersion: bmc.GroupVersion.String(),
			Kind:       "Task",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: bmc.TaskSpec{
			Task: action,
			Connection: bmc.Connection{
				Host: "host",
				Port: 22,
				AuthSecretRef: corev1.SecretReference{
					Name:      secret.Name,
					Namespace: secret.Namespace,
				},
				ProviderOptions: &bmc.ProviderOptions{
					Redfish: &bmc.RedfishOptions{
						Port: 443,
					},
				},
			},
		},
	}
}

func createTaskWithRPC(name string, action bmc.Action, secret *corev1.Secret) *bmc.Task {
	task := &bmc.Task{
		TypeMeta: metav1.TypeMeta{
			APIVersion: bmc.GroupVersion.String(),
			Kind:       "Task",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: bmc.TaskSpec{
			Task: action,
			Connection: bmc.Connection{
				Host: "host",
				Port: 22,
				ProviderOptions: &bmc.ProviderOptions{
					RPC: &bmc.RPCOptions{
						ConsumerURL: "http://127.0.0.1:7777",
					},
				},
			},
		},
	}

	if secret != nil {
		task.Spec.Connection.AuthSecretRef = corev1.SecretReference{
			Name:      secret.Name,
			Namespace: secret.Namespace,
		}

		task.Spec.Connection.ProviderOptions.RPC.HMAC = &bmc.HMACOpts{
			Secrets: bmc.HMACSecrets{
				"sha256": []corev1.SecretReference{
					{
						Name:      secret.Name,
						Namespace: secret.Namespace,
					},
				},
			},
		}
	}

	return task
}

func TestTaskReconcileEvents(t *testing.T) {
	tests := map[string]struct {
		action   bmc.Action
		provider *testProvider
		want     []string
	}{
		"power on": {
			action:   getAction("PowerOn"),
			provider: &testProvider{Powerstate: "on", PowerSetOK: true},
			want:     []string{"Normal PowerActionStarted", "Normal PowerActionCompleted"},
		},
		"power on fails": {
			action:   getAction("PowerOn"),
			provider: &testProvider{ErrPowerStateSet: errors.New("failed to set power state")},
			want:     []string{"Warning PowerActionFailed"},
		},
		"bmc unreachable": {
			action:   getAction("HardOff"),
			provider: &testProvider{ErrOpen: errors.New("failed to open")},
			want:     []string{"Warning PowerActionFailed"},
		},
		"virtual media": {
			action:   getAction("VirtualMedia"),
			provider: &testProvider{VirtualMediaOK: true},
			want:     []string{"Normal VirtualMediaStarted", "Normal VirtualMediaCompleted"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			secret := createSecret()
			task := createTask("events", tt.action, secret)
			cluster := newClientBuilder().WithObjects(task, secret).Build()
			recorder := events.NewFakeRecorder(10)
			reconciler := controller.NewTaskReconciler(cluster, recorder, newTestClient(tt.provider))
			request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: task.Namespace, Name: task.Name}}

			// The second reconcile checks on the action started by the first; a failed Task is left alone.
			_, _ = reconciler.Reconcile(context.Background(), request)
			_, _ = reconciler.Reconcile(context.Background(), request)
			close(recorder.Events)

			var got []string
			for e := range recorder.Events {
				// Keep "<type> <reason>", the note is free text.
				f := strings.Fields(e)
				got = append(got, f[0]+" "+f[1])
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("unexpected events (-want +got):\n%s", diff)
			}
		})
	}
}

// capturingRecorder is an events.EventRecorder that keeps each Event's regarding and related
// object as "<kind>/<name>", "-" standing in for a nil related object.
type capturingRecorder struct{ targets []string }

func (c *capturingRecorder) Eventf(regarding, related runtime.Object, _, _, _, _ string, _ ...any) {
	name := func(o runtime.Object) string {
		if o == nil {
			return "-"
		}
		return fmt.Sprintf("%T/%s", o, o.(metav1.Object).GetName())
	}
	c.targets = append(c.targets, name(regarding)+" "+name(related))
}

func TestTaskEventTarget(t *testing.T) {
	hwLinkedTo := func(name, machine string) *tinkerbell.Hardware {
		return &tinkerbell.Hardware{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       tinkerbell.HardwareSpec{BMCRef: &corev1.TypedLocalObjectReference{Kind: "Machine", Name: machine}},
		}
	}
	tests := map[string]struct {
		owned    bool
		hardware []client.Object
		// unindexed makes field-selector Lists fail, as they do when inventory collection is disabled.
		unindexed bool
		want      string
	}{
		"linked Hardware": {
			owned:    true,
			hardware: []client.Object{hwLinkedTo("hw1", "machine1"), hwLinkedTo("other", "machine2")},
			want:     "*tinkerbell.Hardware/hw1 *bmc.Task/events",
		},
		"linked Hardware without the field index": {
			owned:     true,
			hardware:  []client.Object{hwLinkedTo("hw1", "machine1"), hwLinkedTo("other", "machine2")},
			unindexed: true,
			want:      "*tinkerbell.Hardware/hw1 *bmc.Task/events",
		},
		"no linked Hardware falls back to the Machine": {
			owned:    true,
			hardware: []client.Object{hwLinkedTo("other", "machine2")},
			want:     "*bmc.Machine/machine1 *bmc.Task/events",
		},
		"ambiguous Hardware falls back to the Machine": {
			owned:    true,
			hardware: []client.Object{hwLinkedTo("hw1", "machine1"), hwLinkedTo("hw2", "machine1")},
			want:     "*bmc.Machine/machine1 *bmc.Task/events",
		},
		"no owning Job falls back to the Task": {
			want: "*bmc.Task/events -",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			secret := createSecret()
			task := createTask("events", getAction("PowerOn"), secret)
			objs := []client.Object{task, secret}
			objs = append(objs, tt.hardware...)
			if tt.owned {
				job := &bmc.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "job1", Namespace: "default"},
					Spec:       bmc.JobSpec{MachineRef: bmc.MachineRef{Name: "machine1", Namespace: "default"}},
				}
				machine := &bmc.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine1", Namespace: "default"}}
				task.OwnerReferences = []metav1.OwnerReference{{APIVersion: bmc.GroupVersion.String(), Kind: "Job", Name: job.Name}}
				objs = append(objs, job, machine)
			}
			c := newClientBuilder().WithObjects(objs...).Build()
			if tt.unindexed {
				c = interceptor.NewClient(c, interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						lo := &client.ListOptions{}
						lo.ApplyOptions(opts)
						if lo.FieldSelector != nil && !lo.FieldSelector.Empty() {
							return errors.New("index does not exist")
						}
						return c.List(ctx, list, opts...)
					},
				})
			}
			recorder := &capturingRecorder{}
			reconciler := controller.NewTaskReconciler(c, recorder, newTestClient(&testProvider{Powerstate: "on", PowerSetOK: true}))

			if _, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: task.Namespace, Name: task.Name}}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff([]string{tt.want}, recorder.targets); diff != "" {
				t.Fatalf("unexpected event target (-want +got):\n%s", diff)
			}
		})
	}
}
