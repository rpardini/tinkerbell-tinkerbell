package hardware

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
)

// Event reasons recorded against Hardware while netbooting.
const (
	ReasonNetbootServed     = "NetbootServed"
	ReasonNetbootNotAllowed = "NetbootNotAllowed"
	ReasonNetbootFailed     = "NetbootFailed"
)

// EventActionNetboot is the Event action for everything Smee records while netbooting.
const EventActionNetboot = "Netboot"

// Event records a netboot Event against the Hardware that hw was translated from.
// It is a no-op when rec is nil (backends without a Kubernetes API) or hw carries no Hardware.
func Event(rec events.EventRecorder, hw Info, eventtype, reason, note string, args ...any) {
	if rec == nil || hw.Hardware == nil {
		return
	}
	rec.Eventf(hw.Hardware, nil, eventtype, reason, EventActionNetboot, note, args...)
}

// Normal records a Normal netboot Event, see Event.
func Normal(rec events.EventRecorder, hw Info, reason, note string, args ...any) {
	Event(rec, hw, corev1.EventTypeNormal, reason, note, args...)
}

// Warning records a Warning netboot Event, see Event.
func Warning(rec events.EventRecorder, hw Info, reason, note string, args ...any) {
	Event(rec, hw, corev1.EventTypeWarning, reason, note, args...)
}
