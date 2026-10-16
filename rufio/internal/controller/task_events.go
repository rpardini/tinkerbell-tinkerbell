package controller

import (
	"context"
	"fmt"
	"slices"

	"github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	"github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Suffixes of the Event reasons recorded for a Task, prefixed with the kind of BMC action it runs,
// e.g. PowerActionStarted, BootDeviceCompleted, VirtualMediaFailed.
const (
	eventReasonSuffixStarted   = "Started"
	eventReasonSuffixCompleted = "Completed"
	eventReasonSuffixFailed    = "Failed"
)

// describeAction returns the kind of BMC action a Task runs, used as the Event action and reason
// prefix, and a human readable description of it.
func describeAction(a bmc.Action) (kind, description string) {
	switch {
	case a.PowerAction != nil:
		return "PowerAction", fmt.Sprintf("power %s", *a.PowerAction)
	case a.OneTimeBootDeviceAction != nil && len(a.OneTimeBootDeviceAction.Devices) > 0: //nolint:staticcheck // oneTimeBootDeviceAction is deprecated but still supported for backward compatibility.
		return "BootDevice", fmt.Sprintf("set one time boot device %s (efi: %t)", a.OneTimeBootDeviceAction.Devices[0], a.OneTimeBootDeviceAction.EFIBoot) //nolint:staticcheck // see above.
	case a.BootDevice != nil:
		return "BootDevice", fmt.Sprintf("set boot device %s (persistent: %t, efi: %t)", a.BootDevice.Device, a.BootDevice.Persistent, a.BootDevice.EFIBoot)
	case a.VirtualMediaAction != nil:
		if a.VirtualMediaAction.MediaURL == "" {
			return "VirtualMedia", fmt.Sprintf("eject virtual media %s", a.VirtualMediaAction.Kind)
		}
		return "VirtualMedia", fmt.Sprintf("insert virtual media %s from %s", a.VirtualMediaAction.Kind, a.VirtualMediaAction.MediaURL)
	default:
		return "BMCTask", "no action"
	}
}

// eventTarget returns the object a Task's Events are recorded against, and the related object.
// That is the Hardware linked (via spec.bmcRef) to the Machine targeted by the Task's owning Job,
// so a machine's history is in one place, with the Task as the related object. It falls back to
// the Machine when no single Hardware links to it, and to the Task when the Machine can't be found.
func (r *TaskReconciler) eventTarget(ctx context.Context, task *bmc.Task) (regarding, related runtime.Object) {
	for _, ref := range task.OwnerReferences {
		if ref.Kind != "Job" {
			continue
		}
		job := &bmc.Job{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: ref.Name}, job); err != nil {
			break
		}
		machine := &bmc.Machine{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: job.Spec.MachineRef.Namespace, Name: job.Spec.MachineRef.Name}, machine); err != nil {
			break
		}
		if hw := r.linkedHardware(ctx, machine); hw != nil {
			return hw, task
		}
		return machine, task
	}
	return task, nil
}

// linkedHardware returns the single Hardware whose spec.bmcRef points at the Machine, or nil.
func (r *TaskReconciler) linkedHardware(ctx context.Context, machine *bmc.Machine) *tinkerbell.Hardware {
	var list tinkerbell.HardwareList
	if err := r.client.List(ctx, &list, client.InNamespace(machine.Namespace), client.MatchingFields{hardwareBMCRefIndexKey: machine.Name}); err != nil {
		// The field index is only registered when inventory collection is enabled; filter by hand without it.
		list = tinkerbell.HardwareList{}
		if err := r.client.List(ctx, &list, client.InNamespace(machine.Namespace)); err != nil {
			return nil
		}
	}
	var found *tinkerbell.Hardware
	for i := range list.Items {
		if !slices.Contains(hardwareBMCRefIndexFunc(&list.Items[i]), machine.Name) {
			continue
		}
		if found != nil {
			return nil // ambiguous, see findLinkedHardware
		}
		found = &list.Items[i]
	}
	return found
}

// recordEvent records an Event about the Task's BMC action. The reason is the action kind followed by suffix.
func (r *TaskReconciler) recordEvent(ctx context.Context, task *bmc.Task, eventtype, suffix, note string, args ...any) {
	if r.recorder == nil {
		return
	}
	kind, description := describeAction(task.Spec.Task)
	regarding, related := r.eventTarget(ctx, task)
	r.recorder.Eventf(regarding, related, eventtype, kind+suffix, kind, "%s (Task %s, BMC %s): %s",
		description, task.Name, task.Spec.Connection.Host, fmt.Sprintf(note, args...))
}

func (r *TaskReconciler) recordStarted(ctx context.Context, task *bmc.Task) {
	r.recordEvent(ctx, task, corev1.EventTypeNormal, eventReasonSuffixStarted, "sent to the BMC")
}

func (r *TaskReconciler) recordCompleted(ctx context.Context, task *bmc.Task) {
	r.recordEvent(ctx, task, corev1.EventTypeNormal, eventReasonSuffixCompleted, "completed")
}

func (r *TaskReconciler) recordFailed(ctx context.Context, task *bmc.Task, err error) {
	r.recordEvent(ctx, task, corev1.EventTypeWarning, eventReasonSuffixFailed, "%v", err)
}
