// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux && test

package amd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func draRenderNode(path string) CDIDeviceNode {
	return CDIDeviceNode{Path: path, Type: "c", Major: 226, Minor: -1}
}

func TestMatchDRADeviceNodesPartitions(t *testing.T) {
	fs, devices := partitionedHost(t)
	matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{
		{Path: "/dev/kfd", Major: 237, Minor: 0},
		{Path: "/dev/dri/card1", Major: 226, Minor: 1},
		draRenderNode("/dev/dri/renderD129"),
		draRenderNode("/dev/dri/renderD130"),
	})
	require.NoError(t, err)
	assert.Equal(t, []*Device{devices[1]}, matched)

	// Multiple physical owners and repeated partition nodes produce one result
	// per physical UUID, not per CDI node or partition index.
	matched, err = MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{
		draRenderNode("/dev/dri/renderD130"),
		draRenderNode("/dev/dri/renderD128"),
		draRenderNode("/dev/dri/renderD129"),
	})
	require.NoError(t, err)
	assert.Equal(t, []*Device{devices[1], devices[0]}, matched)
}

func TestMatchDRADeviceNodesValidation(t *testing.T) {
	fs, devices := partitionedHost(t)
	for _, tc := range []struct {
		name  string
		node  CDIDeviceNode
		valid bool
	}{
		{"omitted numbers", CDIDeviceNode{Path: "/dev/dri/renderD129", Major: -1, Minor: -1}, true},
		{"explicit numbers", CDIDeviceNode{Path: "/dev/dri/renderD129", Type: "c", Major: 226, Minor: 129}, true},
		{"host path owns", CDIDeviceNode{Path: "/container/render", HostPath: "/dev/dri/renderD129", Major: -1, Minor: -1}, true},
		{"container path cannot override host", CDIDeviceNode{Path: "/dev/dri/renderD129", HostPath: "/dev/dri/renderD250", Major: -1, Minor: -1}, false},
		{"explicit zero major", CDIDeviceNode{Path: "/dev/dri/renderD129", Major: 0, Minor: -1}, false},
		{"explicit zero minor", CDIDeviceNode{Path: "/dev/dri/renderD129", Major: 226, Minor: 0}, false},
		{"different minor", CDIDeviceNode{Path: "/dev/dri/renderD129", Major: 226, Minor: 130}, false},
		{"block device", CDIDeviceNode{Path: "/dev/dri/renderD129", Type: "b", Major: 226, Minor: 129}, false},
		{"negative major", CDIDeviceNode{Path: "/dev/dri/renderD129", Major: -2, Minor: -1}, false},
		{"leading zero", draRenderNode("/dev/dri/renderD0129"), false},
		{"path traversal", draRenderNode("/dev/dri/../dri/renderD129"), false},
		{"suffix", draRenderNode("/dev/dri/renderD129/other"), false},
		{"nonrender minor", draRenderNode("/dev/dri/renderD1"), false},
		{"unsupported node", draRenderNode("/dev/nvidia0"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{tc.node})
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, []*Device{devices[1]}, matched)
			} else {
				require.Error(t, err)
				assert.Empty(t, matched)
			}
		})
	}
}

func TestMatchDRADeviceNodesPartialResolution(t *testing.T) {
	fs, devices := partitionedHost(t)
	matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{
		draRenderNode("/dev/dri/renderD129"),
		draRenderNode("/dev/dri/renderD250"), // XCP path alone does not identify a GPU
		draRenderNode("/dev/dri/renderD999"),
	})
	require.Error(t, err)
	assert.Equal(t, []*Device{devices[1]}, matched)
	for _, nodes := range [][]CDIDeviceNode{
		nil,
		{{Path: "/dev/kfd", Major: 237, Minor: 0}, {Path: "/dev/dri/card1", Major: 226, Minor: 1}},
	} {
		matched, err := MatchDRADeviceNodes(fs.Root, devices, nodes)
		require.Error(t, err)
		assert.Empty(t, matched)
	}
}

func TestMatchDRADeviceNodesDirectPCIFallback(t *testing.T) {
	fs := NewFakeSysfs(t)
	fn0 := fs.AddPCIDevice("0000:83:00.0", "amdgpu", MI300XAttributes(""))
	fn1 := fs.AddPCIDevice("0000:83:00.1", "amdgpu", MI300XAttributes("00c0ffee00c0ffee"))
	fs.AddCard("card0", fn0)
	fs.AddCard("card1", fn1)
	fs.AddCard("renderD129", fn1)
	devices, err := Discover(fs.Root)
	require.NoError(t, err)
	matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{draRenderNode("/dev/dri/renderD129")})
	require.NoError(t, err)
	assert.Equal(t, []*Device{devices[1]}, matched)
	assert.Equal(t, "0000:83:00.1", matched[0].PCIBusID)

	// KFD's function bits may encode an XCP ID; a direct DRM PCI function is
	// sufficient to disambiguate, without flattening .1 to .0.
	fs.AddKFDNode(1, 4101, 0, 0x8303, 90402)
	fs.SetKFDRenderMinor(1, 129)
	matched, err = MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{draRenderNode("/dev/dri/renderD129")})
	require.NoError(t, err)
	assert.Equal(t, []*Device{devices[1]}, matched)
}

func TestMatchDRADeviceNodesFallbackBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*FakeSysfs, string) string
	}{
		{"platform only", func(fs *FakeSysfs, _ string) string { return fs.AddPlatformDevice("amdgpu_xcp_1") }},
		{"PCI ancestor of XCP", func(fs *FakeSysfs, pci string) string {
			target := filepath.Join(pci, "amdgpu_xcp_1")
			require.NoError(t, os.MkdirAll(target, 0o755))
			return target
		}},
		{"other PCI function", func(fs *FakeSysfs, _ string) string {
			return fs.AddPCIDevice("0000:83:00.1", "amdgpu", MI300XAttributes(""))
		}},
		{"other vendor", func(fs *FakeSysfs, _ string) string {
			attrs := MI300XAttributes("")
			attrs["vendor"] = "0x10de\n"
			return fs.AddPCIDevice("0000:84:00.0", "nvidia", attrs)
		}},
		{"KFD disagreement", func(fs *FakeSysfs, pci string) string {
			fs.AddKFDNode(1, 4101, 0, 0x8400, 90402)
			fs.SetKFDRenderMinor(1, 129)
			return pci
		}},
		{"invalid KFD location", func(fs *FakeSysfs, pci string) string {
			fs.AddKFDNode(1, 4101, 0, 0x8300, 90402)
			fs.SetKFDRenderMinor(1, 129)
			fs.SetKFDProperty(1, "location_id", 0x10000)
			return pci
		}},
		{"conflicting KFD slots", func(fs *FakeSysfs, pci string) string {
			fs.AddKFDNode(1, 4101, 0, 0x8300, 90402)
			fs.SetKFDRenderMinor(1, 129)
			fs.AddKFDNode(2, 4102, 0, 0x8400, 90402)
			fs.SetKFDRenderMinor(2, 129)
			return pci
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := NewFakeSysfs(t)
			pci := fs.AddPCIDevice("0000:83:00.0", "amdgpu", MI300XAttributes(""))
			fs.AddCard("card0", pci)
			// Discover before adding the render link so the fallback must examine
			// raw KFD evidence, not just previously accepted ownership.
			devices, err := Discover(fs.Root)
			require.NoError(t, err)
			fs.AddCard("renderD129", tc.setup(fs, pci))
			matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{draRenderNode("/dev/dri/renderD129")})
			require.Error(t, err)
			assert.Empty(t, matched)
		})
	}
}

func TestMatchDRADeviceNodesRejectsAmbiguousKFD(t *testing.T) {
	fs, _ := partitionedHost(t)
	fs.SetKFDRenderMinor(1, 129) // two physical slots claim the same minor
	devices, err := Discover(fs.Root)
	require.NoError(t, err)
	matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{
		draRenderNode("/dev/dri/renderD129"),
		draRenderNode("/dev/dri/renderD130"),
	})
	require.Error(t, err)
	assert.Equal(t, []*Device{devices[1]}, matched)
}

func TestMatchDRADeviceNodesDoesNotWeakenProcessTopology(t *testing.T) {
	fs := NewFakeSysfs(t)
	pci := fs.AddPCIDevice("0000:83:00.0", "amdgpu", MI300XAttributes(""))
	fs.AddCard("card0", pci)
	fs.AddCard("renderD129", pci)
	fs.AddKFDNode(1, 4101, 0, 0x8300, 90402)
	fs.SetKFDRenderMinor(1, 129)
	fs.AddKFDNode(2, 4102, 0, 0x8301, 90402)
	fs.SetKFDProperty(2, "location_id", 0x10000)
	fs.AddKFDProcess(123, 4101, 1024)
	devices, err := Discover(fs.Root)
	require.Error(t, err)
	matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{draRenderNode("/dev/dri/renderD129")})
	require.NoError(t, err)
	assert.Equal(t, devices, matched)
	usage, complete, err := ReadProcessMemory(fs.Root, devices)
	require.NoError(t, err)
	assert.False(t, complete)
	assert.Empty(t, usage)
}

func TestMatchDRADeviceNodesRejectsDiscardedKFD(t *testing.T) {
	fs := NewFakeSysfs(t)
	first := fs.AddPCIDevice("0000:83:00.0", "amdgpu", MI300XAttributes(""))
	second := fs.AddPCIDevice("0000:84:00.0", "amdgpu", MI300XAttributes(""))
	fs.AddCard("card0", first)
	fs.AddCard("card1", second)
	fs.AddCard("renderD129", first)
	// Discover rejects this node rather than publishing contradictory ownership.
	// A fallback must not reinterpret that missing renderMinors entry as no KFD.
	fs.AddKFDNode(1, 4101, 0, 0x8400, 90402)
	fs.SetKFDRenderMinor(1, 129)
	devices, err := Discover(fs.Root)
	require.Error(t, err)
	require.Len(t, devices, 2)
	matched, err := MatchDRADeviceNodes(fs.Root, devices, []CDIDeviceNode{draRenderNode("/dev/dri/renderD129")})
	require.Error(t, err)
	assert.Empty(t, matched)
}
