// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux

package amdgpu

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	"github.com/DataDog/datadog-agent/pkg/gpu/amd"
)

type cdiSpecResult struct {
	spec *amd.CDISpec
	err  error
}

// containerEvents replaces only AMD's contribution with the verified physical
// GPU union. Unlike process associations, allocations are fail-closed even on
// partial discovery: neither an old CDI file nor an old render owner is proof
// of current ownership. Metric exclusions belong to the check, not this source.
func (c *collector) containerEvents(devices []*amd.Device) ([]workloadmeta.CollectorEvent, error) {
	var events []workloadmeta.CollectorEvent
	var errs []error
	current := make(map[string]struct{})
	// Cache parsed files and failures only within this pull. Claims and render
	// minors can change independently of the container's allocation resource.
	specs := make(map[string]cdiSpecResult)
	for _, container := range c.store.ListContainers() {
		var uuids []string
		for _, resource := range container.ResolvedAllocatedResources {
			var matched []*amd.Device
			var err error
			switch {
			case resource.Name == amd.DRADriver:
				matched, err = c.draResourceDevices(devices, resource, specs)
			case strings.HasPrefix(resource.Name, amd.ResourcePrefix):
				if dev := amd.MatchDevicePluginID(c.sysRoot, devices, resource.ID); dev != nil {
					matched = []*amd.Device{dev}
				} else {
					err = fmt.Errorf("no discovered AMD GPU matches device plugin ID %q", resource.ID)
				}
			default:
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("container %q (%s), AMD resource %q (%s): %w", container.ID, container.Name, resource.Name, resource.ID, err))
			}
			for _, dev := range matched {
				if !slices.Contains(uuids, dev.UUID) {
					uuids = append(uuids, dev.UUID)
				}
			}
		}
		if len(uuids) == 0 {
			continue
		}
		current[container.ID] = struct{}{}
		events = append(events, workloadmeta.CollectorEvent{
			Source: workloadmeta.SourceAMDGPU,
			Type:   workloadmeta.EventTypeSet,
			Entity: &workloadmeta.Container{EntityID: container.EntityID, GPUDeviceIDs: uuids},
		})
	}
	for id := range c.seenContainerIDs {
		if _, present := current[id]; present {
			continue
		}
		// Never copy runtime allocations into our contribution: after runtime
		// removal an AMD-only survivor has no resources and is retracted here.
		events = append(events, workloadmeta.CollectorEvent{
			Source: workloadmeta.SourceAMDGPU,
			Type:   workloadmeta.EventTypeUnset,
			Entity: &workloadmeta.Container{EntityID: workloadmeta.EntityID{Kind: workloadmeta.KindContainer, ID: id}},
		})
	}
	c.seenContainerIDs = current
	return events, errors.Join(errs...)
}

// draResourceDevices selects exact claim entries. The resource ID is opaque;
// spec-wide and common edits grant access, not physical GPU ownership.
func (c *collector) draResourceDevices(devices []*amd.Device, resource workloadmeta.ContainerAllocatedResource, specs map[string]cdiSpecResult) ([]*amd.Device, error) {
	const kind = "k8s.gpu.amd.com/gpu"
	var matched []*amd.Device
	var errs []error
	hasDevice := false
	for _, qualified := range resource.CdiDevices {
		key, ok := strings.CutPrefix(qualified, kind+"=")
		if !ok {
			errs = append(errs, fmt.Errorf("unsupported CDI device %q", qualified))
			continue
		}
		if key == "common" {
			continue
		}
		hasDevice = true
		uid := amd.CDIClaimUID(key)
		if uid == "" {
			errs = append(errs, fmt.Errorf("invalid AMD CDI claim entry %q", key))
			continue
		}
		filename := "k8s.gpu.amd.com-gpu_" + uid + ".yaml"
		result, cached := specs[filename]
		if !cached {
			result.spec, result.err = amd.ReadCDISpec(c.cdiSpecDirs, filename)
			specs[filename] = result
		}
		if result.err != nil {
			errs = append(errs, fmt.Errorf("read AMD CDI spec %s: %w", filename, result.err))
			continue
		}
		if result.spec.Kind != kind {
			errs = append(errs, fmt.Errorf("AMD CDI spec %s has kind %q, want %q", filename, result.spec.Kind, kind))
			continue
		}
		nodes, present := result.spec.DeviceNodes(key)
		if !present {
			errs = append(errs, fmt.Errorf("AMD CDI spec %s has no entry %q", filename, key))
			continue
		}
		resolved, err := amd.MatchDRADeviceNodes(c.sysRoot, devices, nodes)
		matched = append(matched, resolved...)
		if err != nil {
			errs = append(errs, fmt.Errorf("AMD CDI entry %q: %w", key, err))
		}
	}
	if !hasDevice {
		errs = append(errs, errors.New("AMD DRA resource has no claim device entries"))
	}
	return matched, errors.Join(errs...)
}
