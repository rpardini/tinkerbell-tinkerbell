package attribute

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/go-logr/logr"
	"github.com/jaypipes/ghw"
	"github.com/jaypipes/ghw/pkg/block"
	"github.com/tinkerbell/tinkerbell/pkg/data"
)

// DiscoverAll discovers all attributes. lldpTimeout is passed to DiscoverNetworks.
// Each step's duration is logged because discovery runs before the Agent takes its first Action,
// so a slow step directly delays provisioning.
func DiscoverAll(ctx context.Context, l logr.Logger, lldpTimeout time.Duration) *data.AgentAttributes {
	start := time.Now()
	a := &data.AgentAttributes{
		CPU:               timed(l, "cpu", func() *data.CPU { return DiscoverCPU(l) }),
		Memory:            timed(l, "memory", func() *data.Memory { return DiscoverMemory(l) }),
		BlockDevices:      timed(l, "block", func() []*data.Block { return DiscoverBlockDevices(l) }),
		NetworkInterfaces: timed(l, "network", func() []*data.Network { return DiscoverNetworks(ctx, l, lldpTimeout) }),
		PCIDevices:        timed(l, "pci", func() []*data.PCI { return DiscoverPCI(l) }),
		GPUDevices:        timed(l, "gpu", func() []*data.GPU { return DiscoverGPU(l) }),
		Chassis:           timed(l, "chassis", func() *data.Chassis { return DiscoverChassis(l) }),
		BIOS:              timed(l, "bios", func() *data.BIOS { return DiscoverBIOS(l) }),
		Baseboard:         timed(l, "baseboard", func() *data.Baseboard { return DiscoverBaseboard(l) }),
		Product:           timed(l, "product", func() *data.Product { return DiscoverProduct(l) }),
	}
	l.Info("attribute discovery finished", "duration", time.Since(start).String())
	return a
}

// timed runs f and logs how long it took.
func timed[T any](l logr.Logger, name string, f func() T) T {
	start := time.Now()
	v := f()
	l.Info("attribute discovery step finished", "attribute", name, "duration", time.Since(start).String())
	return v
}

func DiscoverCPU(l logr.Logger) *data.CPU {
	cpu, err := ghw.CPU(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting cpu info", "error", err)
		return nil
	}
	if cpu == nil {
		return new(data.CPU)
	}
	var processors []*data.Processor
	for _, p := range cpu.Processors {
		if p == nil {
			continue
		}
		id, err := safecast.Convert[uint32](p.ID)
		if err != nil {
			id = uint32(0)
		}
		processors = append(processors, &data.Processor{
			ID:           toPtr(id),
			Cores:        toPtr(p.TotalCores),
			Threads:      toPtr(p.TotalHardwareThreads),
			Vendor:       toPtr(p.Vendor),
			Model:        toPtr(p.Model),
			Capabilities: p.Capabilities,
		})
	}

	return &data.CPU{
		TotalCores:   toPtr(cpu.TotalCores),
		TotalThreads: toPtr(cpu.TotalHardwareThreads),
		Processors:   processors,
	}
}

func DiscoverMemory(l logr.Logger) *data.Memory {
	memory, err := ghw.Memory(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting memory info", "error", err)
		return nil
	}
	if memory == nil {
		return new(data.Memory)
	}

	return &data.Memory{
		TotalBytes:  toPtr(memory.TotalPhysicalBytes),
		UsableBytes: toPtr(memory.TotalUsableBytes),
	}
}

func DiscoverBlockDevices(l logr.Logger) []*data.Block {
	b, err := ghw.Block(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting block info", "error", err)
		return nil
	}
	var blockDevices []*data.Block
	for _, d := range b.Disks {
		if d == nil {
			continue
		}
		if d.StorageController != block.StorageControllerLoop && d.StorageController != block.StorageControllerUnknown {
			dev := &data.Block{
				Name:           toPtr(d.Name),
				ControllerType: toPtr(d.StorageController.String()),
				DriveType:      toPtr(d.DriveType.String()),
				Vendor:         toPtr(d.Vendor),
				Model:          toPtr(d.Model),
				WWN:            toPtr(d.WWN),
				SerialNumber:   toPtr(d.SerialNumber),
				Path:           toPtr(d.BusPath),
			}
			if sizeBytes, err := safecast.Convert[int64](d.SizeBytes); err == nil {
				dev.SizeBytes = toPtr(sizeBytes)
			}
			if physicalBlockSizeBytes, err := safecast.Convert[int64](d.PhysicalBlockSizeBytes); err == nil {
				dev.PhysicalBlockSizeBytes = toPtr(physicalBlockSizeBytes)
			}
			blockDevices = append(blockDevices, dev)
		}
	}
	return blockDevices
}

// DiscoverNetworks discovers network interfaces and their LLDP neighbors. It
// waits up to lldpTimeout for each physical interface that has a link to
// report a neighbor. A zero lldpTimeout skips LLDP discovery.
func DiscoverNetworks(ctx context.Context, l logr.Logger, lldpTimeout time.Duration) []*data.Network {
	net, err := ghw.Network(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting network info", "error", err)
		return nil
	}
	var nics []*data.Network
	// Virtual interfaces never receive a switch's LLDP advertisement, but
	// some report a link anyway, so waiting on them always hits the timeout.
	var physical []string
	for _, n := range net.NICs {
		if n == nil {
			continue
		}
		nic := &data.Network{
			Name: toPtr(n.Name),
			Mac:  toPtr(n.MACAddress),
			EnabledCapabilities: func() []string {
				var capabilities []string
				for _, c := range n.Capabilities {
					if c.IsEnabled {
						capabilities = append(capabilities, c.Name)
					}
				}
				return capabilities
			}(),
		}
		if speedMbps := parseSpeedMbps(n.Speed); speedMbps > 0 {
			nic.SpeedMbps = toPtr(speedMbps)
		}
		nics = append(nics, nic)
		if !n.IsVirtual {
			physical = append(physical, n.Name)
		}
	}

	start := time.Now()
	neighbors := newLLDP().neighbors(ctx, l, lldpTimeout, physical)
	withNeighbors := make([]string, 0, len(neighbors))
	for name := range neighbors {
		withNeighbors = append(withNeighbors, name)
	}
	slices.Sort(withNeighbors)
	l.Info("LLDP neighbor discovery finished", "duration", time.Since(start).String(), "timeout", lldpTimeout.String(),
		"physicalInterfaces", physical, "interfacesWithNeighbors", withNeighbors)
	for _, nic := range nics {
		nic.LLDPNeighbors = neighbors[*nic.Name]
	}
	return nics
}

func DiscoverPCI(l logr.Logger) []*data.PCI {
	p, err := ghw.PCI(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting pci info", "error", err)
		return nil
	}
	var pciDevices []*data.PCI
	for _, d := range p.Devices {
		if d == nil {
			continue
		}
		var valueFound bool
		dev := &data.PCI{}
		if d.Vendor != nil {
			dev.Vendor = toPtr(d.Vendor.Name)
			valueFound = true
		}
		if d.Product != nil {
			dev.Product = toPtr(d.Product.Name)
			valueFound = true
		}
		if d.Class != nil {
			dev.Class = toPtr(d.Class.Name)
			valueFound = true
		}
		if d.Driver != "" {
			dev.Driver = toPtr(d.Driver)
			valueFound = true
		}
		if !valueFound {
			continue
		}

		pciDevices = append(pciDevices, dev)
	}
	return pciDevices
}

func DiscoverGPU(l logr.Logger) []*data.GPU {
	g, err := ghw.GPU(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting gpu info", "error", err)
		return nil
	}
	var gpus []*data.GPU
	for _, gc := range g.GraphicsCards {
		if gc == nil {
			continue
		}
		if gc.DeviceInfo == nil {
			continue
		}
		var valueFound bool
		card := &data.GPU{}
		if gc.DeviceInfo.Vendor != nil {
			card.Vendor = toPtr(gc.DeviceInfo.Vendor.Name)
			valueFound = true
		}
		if gc.DeviceInfo.Product != nil {
			card.Product = toPtr(gc.DeviceInfo.Product.Name)
			valueFound = true
		}
		if gc.DeviceInfo.Class != nil {
			card.Class = toPtr(gc.DeviceInfo.Class.Name)
			valueFound = true
		}
		if gc.DeviceInfo.Driver != "" {
			card.Driver = toPtr(gc.DeviceInfo.Driver)
			valueFound = true
		}
		if !valueFound {
			continue
		}

		gpus = append(gpus, card)
	}
	return gpus
}

func DiscoverChassis(l logr.Logger) *data.Chassis {
	chass, err := ghw.Chassis(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting chassis info", "error", err)
		return nil
	}
	if chass == nil {
		return new(data.Chassis)
	}
	return &data.Chassis{
		Serial: toPtr(chass.SerialNumber),
		Vendor: toPtr(chass.Vendor),
	}
}

func DiscoverBIOS(l logr.Logger) *data.BIOS {
	bio, err := ghw.BIOS(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting bios info", "error", err)
		return nil
	}
	if bio == nil {
		return new(data.BIOS)
	}
	return &data.BIOS{
		Vendor:      toPtr(bio.Vendor),
		Version:     toPtr(bio.Version),
		ReleaseDate: toPtr(bio.Date),
	}
}

func DiscoverBaseboard(l logr.Logger) *data.Baseboard {
	baseboard, err := ghw.Baseboard(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting baseboard info", "error", err)
		return nil
	}
	if baseboard == nil {
		return new(data.Baseboard)
	}
	return &data.Baseboard{
		Vendor:       toPtr(baseboard.Vendor),
		Product:      toPtr(baseboard.Product),
		Version:      toPtr(baseboard.Version),
		SerialNumber: toPtr(baseboard.SerialNumber),
	}
}

func DiscoverProduct(l logr.Logger) *data.Product {
	product, err := ghw.Product(ghw.WithDisableWarnings())
	if err != nil {
		l.V(1).Info("error getting product info", "error", err)
		return nil
	}
	if product == nil {
		return new(data.Product)
	}
	return &data.Product{
		Name:         toPtr(product.Name),
		Vendor:       toPtr(product.Vendor),
		SerialNumber: toPtr(product.SerialNumber),
	}
}

func toPtr[T any](v T) *T {
	return &v
}

// parseSpeedMbps extracts a leading integer Mbps value from a ghw-reported
// speed string, which varies in format depending on how ghw collected it
// (e.g. "1000" from sysfs, "1000Mb/s" from ethtool, or "-1" from sysfs when
// the link speed is unknown/down). Returns 0 if no leading digits are
// present, or if speed is negative (sysfs's unknown-speed sentinel - a plain
// digit-strip would otherwise turn "-1" into a fabricated 1). Values that
// overflow uint32 after unit scaling clamp to math.MaxUint32 rather than
// wrapping.
func parseSpeedMbps(speed string) uint32 {
	trimmed := strings.TrimSpace(speed)
	if strings.HasPrefix(trimmed, "-") {
		return 0
	}
	end := strings.IndexFunc(trimmed, func(r rune) bool { return r < '0' || r > '9' })
	digits, unit := trimmed, ""
	if end != -1 {
		digits, unit = trimmed[:end], trimmed[end:]
	}
	v, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0
	}
	switch {
	case strings.Contains(unit, "GB"):
		// Gigabytes/sec (case-sensitive: "GB", not "Gb") - 8 bits per byte.
		v *= 8000
	case strings.Contains(unit, "MB"):
		v *= 8
	case strings.Contains(strings.ToLower(unit), "gb"):
		// Gigabits/sec, e.g. ethtool's "10 Gb/s".
		v *= 1000
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}
