// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux

package amd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// MatchDRADeviceNodes resolves CDI host render nodes to physical AMD GPUs.
// KFD owns partition render minors; a direct DRM link to a discovered PCI
// function is the only fallback. Access nodes (kfd and cards) do not establish
// ownership. Verified devices survive errors in other nodes, deduplicated by UUID.
func MatchDRADeviceNodes(sysRoot string, devices []*Device, nodes []CDIDeviceNode) ([]*Device, error) {
	var matched []*Device
	var errs []error
	var kfdSlots map[int]string
	kfdLoaded := false
	renders := 0
	for _, node := range nodes {
		path := node.HostPath
		if path == "" {
			path = node.Path
		}
		if path == "/dev/kfd" || (strings.HasPrefix(path, "/dev/dri/") && cardDirRegex.MatchString(strings.TrimPrefix(path, "/dev/dri/"))) {
			continue
		}
		minorText, render := strings.CutPrefix(path, "/dev/dri/renderD")
		minor, err := strconv.Atoi(minorText)
		if !render || err != nil || minor < 128 || minor > 0xfffff || strconv.Itoa(minor) != minorText {
			errs = append(errs, fmt.Errorf("unsupported AMD CDI host node %q", path))
			continue
		}
		renders++
		if (node.Type != "" && node.Type != "c") || (node.Major != -1 && node.Major != 226) || (node.Minor != -1 && node.Minor != minor) {
			errs = append(errs, fmt.Errorf("contradictory AMD CDI render node %q: type=%q major=%d minor=%d", path, node.Type, node.Major, node.Minor))
			continue
		}

		var owner *Device
		ambiguous := false
		for _, dev := range devices {
			if !slices.Contains(dev.renderMinors, minor) {
				continue
			}
			if owner != nil && owner.PCIBusID != dev.PCIBusID {
				ambiguous = true
				break
			}
			owner = dev
		}
		if ambiguous {
			errs = append(errs, fmt.Errorf("ambiguous KFD ownership of %q", path))
			continue
		}

		target, linkErr := filepath.EvalSymlinks(filepath.Join(sysRoot, "class", "drm", "renderD"+minorText, "device"))
		address := strings.ToLower(filepath.Base(target))
		directPCI := linkErr == nil && pciAddressRegex.MatchString(address)
		if owner != nil {
			if directPCI && (owner.PCIBusID != address || owner.devicePath != target) {
				errs = append(errs, fmt.Errorf("DRM link for %q disagrees with KFD owner %s", path, owner.PCIBusID))
				continue
			}
		} else {
			if !directPCI {
				errs = append(errs, fmt.Errorf("no KFD owner or direct AMD PCI link for %q", path))
				continue
			}
			for _, dev := range devices {
				if dev.PCIBusID == address && dev.devicePath == target {
					owner = dev
					break
				}
			}
			if owner == nil {
				errs = append(errs, fmt.Errorf("%q does not resolve to a discovered AMD PCI function", path))
				continue
			}
		}
		if !kfdLoaded {
			kfdSlots = draKFDRenderSlots(sysRoot)
			kfdLoaded = true
		}
		if slot, available := kfdSlots[minor]; available && slot != owner.PCIBusID[:len(owner.PCIBusID)-1] {
			errs = append(errs, fmt.Errorf("render node %q disagrees with available KFD ownership", path))
			continue
		}
		if !slices.ContainsFunc(matched, func(dev *Device) bool { return dev.UUID == owner.UUID }) {
			matched = append(matched, owner)
		}
	}
	if renders == 0 {
		errs = append(errs, errors.New("AMD CDI entry has no render nodes"))
	}
	return matched, errors.Join(errs...)
}

// draKFDRenderSlots retains readable KFD evidence even when Discover rejected a
// node for a contradictory DRM link. Function bits can denote XCP partitions,
// so only the PCI slot is checked here; the direct DRM link selects the exact
// function. Unknown/unreadable nodes cannot defeat a verified direct PCI link.
// Conflicting or invalid readable evidence is represented by an empty slot.
func draKFDRenderSlots(sysRoot string) map[int]string {
	dir := filepath.Join(sysRoot, "class", "kfd", "kfd", "topology", "nodes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	slots := make(map[int]string)
	for _, entry := range entries {
		if _, err := strconv.ParseUint(entry.Name(), 10, 32); err != nil || !entry.IsDir() {
			continue
		}
		nodeDir := filepath.Join(dir, entry.Name())
		gpuID, err := readUint(filepath.Join(nodeDir, "gpu_id"))
		if err != nil || gpuID == 0 {
			continue
		}
		props, err := readKFDProperties(filepath.Join(nodeDir, "properties"))
		if err != nil {
			continue
		}
		minor, hasMinor := props["drm_render_minor"]
		if !hasMinor || minor < 128 || minor > 0xfffff {
			continue
		}
		domain, hasDomain := props["domain"]
		location, hasLocation := props["location_id"]
		slot := ""
		if hasDomain && hasLocation && domain <= 0xffffffff && location <= 0xffff {
			slot = fmt.Sprintf("%04x:%02x:%02x.", domain, (location>>8)&0xff, (location>>3)&0x1f)
		}
		if previous, exists := slots[int(minor)]; exists && previous != slot {
			slot = ""
		}
		slots[int(minor)] = slot
	}
	return slots
}
