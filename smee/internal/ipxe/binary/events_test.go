package binary

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	"github.com/tinkerbell/tinkerbell/smee/internal/hardware"
	"k8s.io/client-go/tools/events"
)

func TestRouteEvents(t *testing.T) {
	mac := net.HardwareAddr{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}
	clientIP := net.ParseIP("192.168.1.50")
	const serial = "abc123"
	hw := hardware.Info{
		AllowNetboot: true,
		PXELINUX:     hardware.PXELINUX{Config: "pxelinux-body"},
		RPI:          hardware.RPI{SerialNum: serial, FirmwarePath: "rpi4b", ConfigTxt: "config-txt-body"},
		Hardware:     &tinkerbell.Hardware{},
	}
	denied := hw
	denied.AllowNetboot = false

	tests := map[string]struct {
		route    func(hardware.Info, events.EventRecorder) Route
		hw       hardware.Info
		filename string
		want     []string
	}{
		"pxelinux served": {
			route:    pxelinuxRoute(mac),
			hw:       hw,
			filename: "pxelinux.cfg/01-00-01-02-03-04-05",
			want:     []string{"Normal NetbootServed pxelinux.cfg/01-00-01-02-03-04-05 served"},
		},
		"pxelinux not allowed": {
			route:    pxelinuxRoute(mac),
			hw:       denied,
			filename: "pxelinux.cfg/01-00-01-02-03-04-05",
			want:     []string{"Warning NetbootNotAllowed pxelinux.cfg/01-00-01-02-03-04-05 refused, netboot.allowPXE is false"},
		},
		"pxelinux without a Hardware object records nothing": {
			route:    pxelinuxRoute(mac),
			hw:       hardware.Info{AllowNetboot: true, PXELINUX: hw.PXELINUX},
			filename: "pxelinux.cfg/01-00-01-02-03-04-05",
		},
		"rpi config.txt served": {
			route:    rpiRoute(clientIP),
			hw:       hw,
			filename: serial + "/config.txt",
			want:     []string{"Normal NetbootServed RPi " + serial + "/config.txt served"},
		},
		"rpi cmdline.txt records nothing": {
			route:    rpiRoute(clientIP),
			hw:       hardware.Info{AllowNetboot: true, RPI: hw.RPI, OSIE: hardware.OSIE{KernelParams: []string{"rw"}}, Hardware: hw.Hardware},
			filename: serial + "/cmdline.txt",
		},
		"rpi not allowed": {
			route:    rpiRoute(clientIP),
			hw:       denied,
			filename: serial + "/config.txt",
			want:     []string{"Warning NetbootNotAllowed RPi netboot file " + serial + "/config.txt refused, netboot.allowPXE is false"},
		},
		"rpi not allowed for a non-RPi file records nothing": {
			route:    rpiRoute(clientIP),
			hw:       denied,
			filename: "ipxe.efi",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(10)
			req := Request{Filename: tt.filename, Client: net.UDPAddr{IP: clientIP}}
			_, _ = tt.route(tt.hw, recorder).TryServe(context.Background(), req, &bytes.Buffer{})
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

func pxelinuxRoute(mac net.HardwareAddr) func(hardware.Info, events.EventRecorder) Route {
	return func(hw hardware.Info, rec events.EventRecorder) Route {
		return PXELinuxMACRoute{Log: logr.Discard(), Resolver: &fakeResolver{byMAC: map[string]hardware.Info{mac.String(): hw}}, Recorder: rec}
	}
}

func rpiRoute(ip net.IP) func(hardware.Info, events.EventRecorder) Route {
	return func(hw hardware.Info, rec events.EventRecorder) Route {
		return RPiNetbootRoute{Log: logr.Discard(), Resolver: &fakeResolver{byIP: map[string]hardware.Info{ip.String(): hw}}, AssetDir: "/nonexistent", Recorder: rec}
	}
}
