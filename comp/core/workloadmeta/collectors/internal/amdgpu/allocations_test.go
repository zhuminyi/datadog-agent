// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux && test

package amdgpu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/datadog-agent/comp/core/config"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	"github.com/DataDog/datadog-agent/pkg/gpu/amd"
)

const (
	draFixtureFile = "k8s.gpu.amd.com-gpu_11111111-1111-4111-8111-111111111111.yaml"
	draEntry       = "11111111-1111-4111-8111-111111111111-gpu-1-129"
	firstUUID      = "amd-0000-82-00-0"
	secondUUID     = "amd-00c0ffee00c0ffee"
)

func draFixture(t *testing.T) (workloadmeta.ContainerAllocatedResource, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "dra", "container-allocated-resource.json"))
	require.NoError(t, err)
	var resource workloadmeta.ContainerAllocatedResource
	require.NoError(t, json.Unmarshal(data, &resource))
	spec, err := os.ReadFile(filepath.Join("testdata", "dra", draFixtureFile))
	require.NoError(t, err)
	return resource, string(spec)
}

// The recorded claim selects render minor 129, not GPU ordinal 1. Two KFD
// partitions share that physical GPU. No process is using either GPU.
func allocationHost(t *testing.T) *amd.FakeSysfs {
	t.Helper()
	fs := amd.NewFakeSysfs(t)
	for i, pci := range []string{"0000:82:00.0", "0000:83:00.0"} {
		serial := ""
		if i == 1 {
			serial = "00c0ffee00c0ffee"
		}
		fs.AddCard("card"+strconv.Itoa(i), fs.AddPCIDevice(pci, "amdgpu", amd.MI300XAttributes(serial)))
	}
	fs.AddKFDNode(1, 4101, 0, 0x8200, 90402)
	fs.SetKFDRenderMinor(1, 128)
	fs.AddKFDNode(2, 4102, 0, 0x8300, 90402)
	fs.SetKFDRenderMinor(2, 129)
	fs.AddKFDNode(3, 4103, 0, 0x8301, 90402)
	fs.SetKFDRenderMinor(3, 130)
	fs.AddPartitionRenderNode("amdgpu_xcp_1", 129)
	fs.AddPartitionRenderNode("amdgpu_xcp_2", 130)
	return fs
}

func allocatedContainer(id string, resources ...workloadmeta.ContainerAllocatedResource) *workloadmeta.Container {
	return &workloadmeta.Container{
		EntityID:                   workloadmeta.EntityID{Kind: workloadmeta.KindContainer, ID: id},
		EntityMeta:                 workloadmeta.EntityMeta{Name: "name-" + id},
		ResolvedAllocatedResources: resources,
	}
}

func notifyContainer(store workloadmeta.Component, source workloadmeta.Source, eventType workloadmeta.EventType, container *workloadmeta.Container) {
	store.Notify([]workloadmeta.CollectorEvent{{Source: source, Type: eventType, Entity: container}})
}

func assertContainerGPUs(t *testing.T, store workloadmeta.Component, id string, uuids ...string) {
	t.Helper()
	container, err := store.GetContainer(id)
	require.NoError(t, err)
	assert.ElementsMatch(t, uuids, container.GPUDeviceIDs)
}

func TestPullContainerAllocations(t *testing.T) {
	resource, spec := draFixture(t)
	legacyFirst := workloadmeta.ContainerAllocatedResource{Name: "amd.com/gpu", ID: "0000:82:00.0"}
	legacySecond := workloadmeta.ContainerAllocatedResource{Name: "amd.com/cpx_nps4", ID: "amdgpu_xcp_1"}
	for _, tc := range []struct {
		name       string
		spec       string
		resources  []workloadmeta.ContainerAllocatedResource
		uuids      []string
		err        string
		unreadable bool
	}{
		{name: "recorded idle claim with ancillary common", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{resource}, uuids: []string{secondUUID}},
		{name: "opaque resource ID cannot override exact entry", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, ID: "gpu-0-128", CdiDevices: resource.CdiDevices}}, uuids: []string{secondUUID}},
		{name: "wrong opaque entry does not guess index", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, ID: "gpu-0-128", CdiDevices: []string{"k8s.gpu.amd.com/gpu=" + strings.Replace(draEntry, "gpu-1-129", "gpu-0-128", 1)}}}, err: "has no entry"},
		{name: "legacy DRA union and physical dedup", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{legacyFirst, resource, legacySecond, resource}, uuids: []string{firstUUID, secondUUID}},
		{name: "legacy whole GPU", resources: []workloadmeta.ContainerAllocatedResource{legacyFirst}, uuids: []string{firstUUID}},
		{name: "legacy shared partitions dedup", resources: []workloadmeta.ContainerAllocatedResource{legacySecond, {Name: "amd.com/cpx_nps4", ID: "amdgpu_xcp_2"}}, uuids: []string{secondUUID}},
		{name: "unknown legacy ID preserves successful subset", resources: []workloadmeta.ContainerAllocatedResource{legacyFirst, {Name: "amd.com/gpu", ID: "0000:e1:00.0"}}, uuids: []string{firstUUID}, err: "no discovered AMD GPU matches"},
		{name: "missing spec preserves legacy", resources: []workloadmeta.ContainerAllocatedResource{resource, legacyFirst}, uuids: []string{firstUUID}, err: "read AMD CDI spec"},
		{name: "unreadable spec preserves legacy", resources: []workloadmeta.ContainerAllocatedResource{resource, legacyFirst}, uuids: []string{firstUUID}, err: "read AMD CDI spec", unreadable: true},
		{name: "malformed spec preserves legacy", spec: "devices: [", resources: []workloadmeta.ContainerAllocatedResource{resource, legacyFirst}, uuids: []string{firstUUID}, err: "decode CDI spec"},
		{name: "wrong kind", spec: strings.Replace(spec, "kind: k8s.gpu.amd.com/gpu", "kind: k8s.gpu.nvidia.com/gpu", 1), resources: []workloadmeta.ContainerAllocatedResource{resource}, err: "has kind"},
		{name: "exact entry absent", spec: strings.Replace(spec, draEntry, draEntry+"-other", 1), resources: []workloadmeta.ContainerAllocatedResource{resource}, err: "has no entry"},
		{name: "entry without nodes", spec: "kind: k8s.gpu.amd.com/gpu\ndevices:\n  - name: " + draEntry + "\n    containerEdits: {}\n", resources: []workloadmeta.ContainerAllocatedResource{resource}, err: "has no render nodes"},
		{name: "access nodes are not ownership", spec: strings.Split(spec, "            - path: /dev/dri/renderD129")[0], resources: []workloadmeta.ContainerAllocatedResource{resource}, err: "has no render nodes"},
		{name: "partial render resolution", spec: spec + "\n            - path: /dev/dri/renderD999\n              type: c\n              major: 226\n              minor: 999\n", resources: []workloadmeta.ContainerAllocatedResource{resource}, uuids: []string{secondUUID}, err: "no KFD owner or direct AMD PCI link"},
		{name: "unsupported node retains verified subset", spec: spec + "\n            - path: /dev/unknown\n", resources: []workloadmeta.ContainerAllocatedResource{resource}, uuids: []string{secondUUID}, err: "unsupported AMD CDI host node"},
		{name: "unsafe render path", spec: strings.ReplaceAll(spec, "/dev/dri/renderD129", "/dev/dri/../dri/renderD129"), resources: []workloadmeta.ContainerAllocatedResource{resource}, err: "unsupported AMD CDI host node"},
		{name: "contradictory minor preserves legacy", spec: strings.Replace(spec, "minor: 129", "minor: 0", 1), resources: []workloadmeta.ContainerAllocatedResource{resource, legacyFirst}, uuids: []string{firstUUID}, err: "contradictory AMD CDI render node"},
		{name: "contradictory major", spec: strings.Replace(spec, "major: 226\n              minor: 129", "major: 0\n              minor: 129", 1), resources: []workloadmeta.ContainerAllocatedResource{resource}, err: "contradictory AMD CDI render node"},
		{name: "common only", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, CdiDevices: []string{"k8s.gpu.amd.com/gpu=common"}}}, err: "has no claim device entries"},
		{name: "empty DRA resource", resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, ID: "gpu-1-129"}}, err: "has no claim device entries"},
		{name: "unsupported CDI prefix", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, CdiDevices: []string{"k8s.gpu.amd.com/gpu-other=" + draEntry}}}, err: "unsupported CDI device"},
		{name: "unsafe claim UID", resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, CdiDevices: []string{"k8s.gpu.amd.com/gpu=../" + draEntry}}}, err: "invalid AMD CDI claim entry"},
		{name: "successful entry survives missing and invalid entries", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, CdiDevices: []string{"k8s.gpu.amd.com/gpu=common", "k8s.gpu.amd.com/gpu=" + draEntry, "k8s.gpu.amd.com/gpu=" + draEntry + "-missing", "k8s.gpu.amd.com/gpu=bad"}}}, uuids: []string{secondUUID}, err: "has no entry"},
		{name: "other driver ignored", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{{Name: "gpu.nvidia.com", ID: resource.ID, CdiDevices: resource.CdiDevices}}},
		{name: "driver prefix is not accepted", spec: spec, resources: []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver + ".other", ID: resource.ID, CdiDevices: resource.CdiDevices}}},
		{name: "NVIDIA legacy ignored", resources: []workloadmeta.ContainerAllocatedResource{{Name: "nvidia.com/gpu", ID: "GPU-nvidia"}}},
		{name: "spec-wide access cannot supply ownership", spec: "kind: k8s.gpu.amd.com/gpu\ncontainerEdits:\n  deviceNodes:\n    - path: /dev/dri/renderD129\ndevices:\n  - name: " + draEntry + "\n    containerEdits: {}\n", resources: []workloadmeta.ContainerAllocatedResource{resource}, err: "has no render nodes"},
		{name: "no resources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			c := newTestCollector(t, store, allocationHost(t).Root)
			c.cdiSpecDirs = []string{t.TempDir()}
			specPath := filepath.Join(c.cdiSpecDirs[0], draFixtureFile)
			if tc.spec != "" {
				require.NoError(t, os.WriteFile(specPath, []byte(tc.spec), 0o644))
			}
			if tc.unreadable {
				// A directory fails as a file even when the test runs as root.
				require.NoError(t, os.Mkdir(specPath, 0o755))
			}
			container := allocatedContainer("workload", tc.resources...)
			notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
			for range 2 {
				err := c.Pull(context.Background())
				if tc.err == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tc.err)
					assert.ErrorContains(t, err, `container "workload" (name-workload), AMD resource`)
				}
				assertContainerGPUs(t, store, container.ID, tc.uuids...)
				stored, err := store.GetContainer(container.ID)
				require.NoError(t, err)
				assert.Equal(t, container.ResolvedAllocatedResources, stored.ResolvedAllocatedResources)
				assert.Equal(t, container.Name, stored.Name)
			}
			for _, gpu := range store.ListGPUs() {
				assert.Empty(t, gpu.ActivePIDs, "idle allocation must not invent active processes")
			}
		})
	}
}

func TestPullAllocationsSharePhysicalGPU(t *testing.T) {
	resource, spec := draFixture(t)
	store := newStore(t)
	c := newTestCollector(t, store, allocationHost(t).Root)
	c.cdiSpecDirs = []string{t.TempDir()}
	otherEntry := strings.Replace(draEntry, "gpu-1-129", "gpu-1-130", 1)
	spec += "\n    - name: " + otherEntry + "\n      containerEdits:\n        deviceNodes:\n            - path: /dev/dri/renderD130\n"
	require.NoError(t, os.WriteFile(filepath.Join(c.cdiSpecDirs[0], draFixtureFile), []byte(spec), 0o644))
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, allocatedContainer("first-partition", resource))
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, allocatedContainer("second-partition", workloadmeta.ContainerAllocatedResource{Name: amd.DRADriver, CdiDevices: []string{"k8s.gpu.amd.com/gpu=" + otherEntry}}))
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, allocatedContainer("legacy-partition", workloadmeta.ContainerAllocatedResource{Name: "amd.com/cpx_nps4", ID: "amdgpu_xcp_2"}))
	require.NoError(t, c.Pull(context.Background()))
	for _, id := range []string{"first-partition", "second-partition", "legacy-partition"} {
		assertContainerGPUs(t, store, id, secondUUID)
	}
}

func TestPullAllocationsIgnoreMetricExclusions(t *testing.T) {
	resource, spec := draFixture(t)
	store := newStore(t)
	cfg := config.NewMock(t)
	cfg.SetInTest("gpu.enabled", true)
	cfg.SetInTest("gpu.amd.enabled", true)
	cfg.SetInTest("gpu.excluded_devices", []string{secondUUID})
	c := newCollector(cfg, allocationHost(t).Root)
	require.NoError(t, c.Start(context.Background(), store))
	c.cdiSpecDirs = []string{t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(c.cdiSpecDirs[0], draFixtureFile), []byte(spec), 0o644))
	owner := &workloadmeta.EntityID{Kind: workloadmeta.KindKubernetesPod, ID: "agent-pod"}
	for _, id := range []string{"agent", "agent-sidecar"} {
		container := allocatedContainer(id, resource)
		container.Owner = owner
		notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
	}
	store.Notify([]workloadmeta.CollectorEvent{{
		Source: workloadmeta.SourceProcessCollector,
		Type:   workloadmeta.EventTypeSet,
		Entity: &workloadmeta.Process{
			EntityID: workloadmeta.EntityID{Kind: workloadmeta.KindProcess, ID: strconv.Itoa(os.Getpid())},
			Pid:      int32(os.Getpid()),
			Owner:    &workloadmeta.EntityID{Kind: workloadmeta.KindContainer, ID: "agent"},
		},
	}})
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, "agent", secondUUID)
	assertContainerGPUs(t, store, "agent-sidecar", secondUUID)
}

func TestPullAllocationFreshness(t *testing.T) {
	resource, spec := draFixture(t)
	fs := allocationHost(t)
	store := newStore(t)
	c := newTestCollector(t, store, fs.Root)
	c.cdiSpecDirs = []string{t.TempDir()}
	specPath := filepath.Join(c.cdiSpecDirs[0], draFixtureFile)
	container := allocatedContainer("old", resource)
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
	run := func(wantError string, uuids ...string) {
		t.Helper()
		err := c.Pull(context.Background())
		if wantError == "" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, wantError)
		}
		assertContainerGPUs(t, store, container.ID, uuids...)
	}
	// A failed read is not retained; neither is a last-known successful mapping.
	run("read AMD CDI spec")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o644))
	run("", secondUUID)
	// Removing the exact entry retracts ownership even if a sibling still grants access.
	require.NoError(t, os.WriteFile(specPath, []byte(strings.Replace(spec, draEntry, draEntry+"-other", 1)), 0o644))
	run("has no entry")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o644))
	run("", secondUUID)
	require.NoError(t, os.Remove(specPath))
	run("read AMD CDI spec")
	// Another filename with the exact entry cannot substitute for the claim file.
	require.NoError(t, os.WriteFile(filepath.Join(c.cdiSpecDirs[0], "unrelated.yaml"), []byte(spec), 0o644))
	run("read AMD CDI spec")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o644))
	run("", secondUUID)
	changed := strings.ReplaceAll(spec, "renderD129", "renderD128")
	changed = strings.Replace(changed, "minor: 129", "minor: 128", 1)
	require.NoError(t, os.WriteFile(specPath, []byte(changed), 0o644))
	run("", firstUUID)
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o644))
	run("", secondUUID)
	// Repartitioning reuses minor 129 while the CDI file stays unchanged.
	fs.SetKFDRenderMinor(1, 129)
	fs.SetKFDRenderMinor(2, 128)
	run("", firstUUID)
	cleared := *container
	cleared.ResolvedAllocatedResources = []workloadmeta.ContainerAllocatedResource{{Name: amd.DRADriver, CdiDevices: []string{"k8s.gpu.amd.com/gpu=common"}}}
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, &cleared)
	run("has no claim device entries")
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
	run("", firstUUID)
	cleared.ResolvedAllocatedResources = nil
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, &cleared)
	run("")
	// The source has already retracted; deleting runtime must leave no survivor.
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeUnset, &cleared)
	require.NoError(t, c.Pull(context.Background()))
	_, err := store.GetContainer(container.ID)
	assert.Error(t, err)
	replacement := *container
	replacement.EntityID.ID = "new"
	replacement.EntityMeta.Name = "name-new"
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, &replacement)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, "new", firstUUID)
}

func TestPullAllocationSourcesRemainIndependent(t *testing.T) {
	resource, spec := draFixture(t)
	store := newStore(t)
	c := newTestCollector(t, store, allocationHost(t).Root)
	c.cdiSpecDirs = []string{t.TempDir()}
	specPath := filepath.Join(c.cdiSpecDirs[0], draFixtureFile)
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o644))
	container := allocatedContainer("mixed", resource)
	container.GPUDeviceIDs = []string{"GPU-runtime"}
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
	nvidia := &workloadmeta.Container{EntityID: container.EntityID, GPUDeviceIDs: []string{"GPU-nvidia"}}
	notifyContainer(store, workloadmeta.SourceNVML, workloadmeta.EventTypeSet, nvidia)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, "GPU-runtime", "GPU-nvidia", secondUUID)
	changed := strings.ReplaceAll(spec, "renderD129", "renderD128")
	changed = strings.Replace(changed, "minor: 129", "minor: 128", 1)
	require.NoError(t, os.WriteFile(specPath, []byte(changed), 0o644))
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, "GPU-runtime", "GPU-nvidia", firstUUID)
	// AMD failure retracts only AMD, without suppressing NVIDIA or runtime IDs.
	require.NoError(t, os.Remove(specPath))
	require.ErrorContains(t, c.Pull(context.Background()), "read AMD CDI spec")
	assertContainerGPUs(t, store, container.ID, "GPU-runtime", "GPU-nvidia")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o644))
	require.NoError(t, c.Pull(context.Background()))
	notifyContainer(store, workloadmeta.SourceNVML, workloadmeta.EventTypeUnset, nvidia)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, "GPU-runtime", secondUUID)
	cleared := *container
	cleared.ResolvedAllocatedResources = nil
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, &cleared)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, "GPU-runtime")
	stored, err := store.GetContainer(container.ID)
	require.NoError(t, err)
	assert.Equal(t, container.Name, stored.Name)
}

func TestPullRetractsAllocationsAfterRuntimeDeletion(t *testing.T) {
	for _, withNVIDIA := range []bool{false, true} {
		t.Run(strconv.FormatBool(withNVIDIA), func(t *testing.T) {
			resource, spec := draFixture(t)
			store := newStore(t)
			c := newTestCollector(t, store, allocationHost(t).Root)
			c.cdiSpecDirs = []string{t.TempDir()}
			require.NoError(t, os.WriteFile(filepath.Join(c.cdiSpecDirs[0], draFixtureFile), []byte(spec), 0o644))
			container := allocatedContainer("deleted", resource)
			notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
			nvidia := &workloadmeta.Container{EntityID: container.EntityID, GPUDeviceIDs: []string{"GPU-nvidia"}}
			if withNVIDIA {
				notifyContainer(store, workloadmeta.SourceNVML, workloadmeta.EventTypeSet, nvidia)
			}
			require.NoError(t, c.Pull(context.Background()))
			uuids := []string{secondUUID}
			if withNVIDIA {
				uuids = append(uuids, "GPU-nvidia")
			}
			assertContainerGPUs(t, store, container.ID, uuids...)
			// AMD's minimal publication must not keep copied allocations alive.
			notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeUnset, container)
			for range 2 {
				require.NoError(t, c.Pull(context.Background()))
				if withNVIDIA {
					assertContainerGPUs(t, store, container.ID, "GPU-nvidia")
				} else {
					_, err := store.GetContainer(container.ID)
					assert.Error(t, err)
				}
			}
			if withNVIDIA {
				notifyContainer(store, workloadmeta.SourceNVML, workloadmeta.EventTypeUnset, nvidia)
				_, err := store.GetContainer(container.ID)
				assert.Error(t, err)
			}
		})
	}
}

func TestPullAllocationsPublishSuccessfulSubsetOnDiscoveryError(t *testing.T) {
	resource, spec := draFixture(t)
	fs := allocationHost(t)
	fs.AddKFDProcess(100, 4102, 1024)
	store := newStore(t)
	c := newTestCollector(t, store, fs.Root)
	c.cdiSpecDirs = []string{t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(c.cdiSpecDirs[0], draFixtureFile), []byte(spec), 0o644))
	container := allocatedContainer("partial", workloadmeta.ContainerAllocatedResource{Name: "amd.com/gpu", ID: "0000:82:00.0"}, resource)
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, firstUUID, secondUUID)
	fs.WriteFiles(filepath.Join(fs.Root, "devices/pci0000:00/0000:83:00.0"), map[string]string{"device": "invalid\n"})
	err := c.Pull(context.Background())
	require.ErrorContains(t, err, "PCI device ID")
	assert.ErrorContains(t, err, `container "partial" (name-partial), AMD resource "gpu.amd.com" (gpu-1-129)`)
	assertContainerGPUs(t, store, container.ID, firstUUID)
	// GPU/process partial-topology guards remain separate from allocations.
	gpu, err := store.GetGPU(secondUUID)
	require.NoError(t, err)
	assert.Equal(t, []int{100}, gpu.ActivePIDs)
	process, err := store.GetProcess(100)
	require.NoError(t, err)
	assert.Equal(t, []workloadmeta.EntityID{{Kind: workloadmeta.KindGPU, ID: secondUUID}}, process.GPUs)
}

func TestPullLegacyAllocationFreshness(t *testing.T) {
	fs := allocationHost(t)
	store := newStore(t)
	c := newTestCollector(t, store, fs.Root)
	container := allocatedContainer("legacy", workloadmeta.ContainerAllocatedResource{Name: "amd.com/gpu", ID: "0000:82:00.0"})
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, container)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, firstUUID)
	reassigned := *container
	reassigned.ResolvedAllocatedResources = []workloadmeta.ContainerAllocatedResource{{Name: "amd.com/cpx_nps4", ID: "amdgpu_xcp_1"}}
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, &reassigned)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, secondUUID)
	// Legacy partition ownership follows current topology, not remembered indices.
	fs.SetKFDRenderMinor(1, 129)
	fs.SetKFDRenderMinor(2, 128)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID, firstUUID)
	require.NoError(t, os.RemoveAll(filepath.Join(fs.Root, "devices", "platform", "amdgpu_xcp_1")))
	require.ErrorContains(t, c.Pull(context.Background()), "no discovered AMD GPU matches")
	assertContainerGPUs(t, store, container.ID)
	reassigned.ResolvedAllocatedResources = nil
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, &reassigned)
	require.NoError(t, c.Pull(context.Background()))
	assertContainerGPUs(t, store, container.ID)
}

func TestPullAllocationErrorsDoNotHideOtherContainers(t *testing.T) {
	resource, _ := draFixture(t)
	store := newStore(t)
	c := newTestCollector(t, store, allocationHost(t).Root)
	c.cdiSpecDirs = []string{t.TempDir()}
	legacy := workloadmeta.ContainerAllocatedResource{Name: "amd.com/gpu", ID: "0000:82:00.0"}
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, allocatedContainer("failed", resource))
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, allocatedContainer("partial", resource, legacy))
	notifyContainer(store, workloadmeta.SourceRuntime, workloadmeta.EventTypeSet, allocatedContainer("successful", legacy))
	err := c.Pull(context.Background())
	require.ErrorContains(t, err, "read AMD CDI spec")
	assert.ErrorContains(t, err, `container "failed" (name-failed), AMD resource "gpu.amd.com" (gpu-1-129)`)
	assert.ErrorContains(t, err, `container "partial" (name-partial), AMD resource "gpu.amd.com" (gpu-1-129)`)
	assertContainerGPUs(t, store, "failed")
	assertContainerGPUs(t, store, "partial", firstUUID)
	assertContainerGPUs(t, store, "successful", firstUUID)
}
