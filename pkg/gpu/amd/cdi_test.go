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

	"github.com/stretchr/testify/require"
)

func writeAMDCDISpec(t *testing.T, dir, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claim.yaml"), []byte(contents), 0o600))
}

func TestReadCDISpecSelectsExactEntry(t *testing.T) {
	dir := t.TempDir()
	writeAMDCDISpec(t, dir, `kind: k8s.gpu.amd.com/gpu
devices:
- name: allocated
  containerEdits:
    deviceNodes:
    - path: /container/render
      hostPath: /dev/dri/renderD129
      type: c
      major: 226
      minor: 129
    - path: /dev/kfd
- name: allocated-sibling
  containerEdits:
    deviceNodes:
    - path: /dev/dri/renderD130
- name: empty
- name: allocated
  containerEdits:
    deviceNodes:
    - path: /dev/dri/renderD131
containerEdits:
  deviceNodes:
  - path: /dev/dri/renderD132
`)
	spec, err := ReadCDISpec([]string{dir}, "claim.yaml")
	require.NoError(t, err)
	require.Equal(t, "k8s.gpu.amd.com/gpu", spec.Kind)
	nodes, ok := spec.DeviceNodes("allocated")
	require.True(t, ok)
	require.Equal(t, []CDIDeviceNode{
		{Path: "/container/render", HostPath: "/dev/dri/renderD129", Type: "c", Major: 226, Minor: 129},
		{Path: "/dev/kfd", Major: -1, Minor: -1},
	}, nodes)
	nodes, ok = spec.DeviceNodes("empty")
	require.True(t, ok)
	require.Empty(t, nodes)
	nodes, ok = spec.DeviceNodes("allocate")
	require.False(t, ok)
	require.Nil(t, nodes)
}

func TestReadCDISpecOptionalDeviceNumbers(t *testing.T) {
	dir := t.TempDir()
	writeAMDCDISpec(t, dir, `devices:
- name: device
  containerEdits:
    deviceNodes:
    - path: /dev/dri/renderD128
      major: 0
      minor: 0
    - path: /dev/dri/renderD129
    - minor: 130
      major: 226
      gid: 0
      uid: 0
      permissions: rw
      type: c
      hostPath: /dev/dri/renderD130
      path: /container/render
    - path: /dev/dri/renderD131
      major: 226
`)
	spec, err := ReadCDISpec([]string{dir}, "claim.yaml")
	require.NoError(t, err)
	nodes, ok := spec.DeviceNodes("device")
	require.True(t, ok)
	require.Equal(t, []CDIDeviceNode{
		{Path: "/dev/dri/renderD128", Major: 0, Minor: 0},
		{Path: "/dev/dri/renderD129", Major: -1, Minor: -1},
		{Path: "/container/render", HostPath: "/dev/dri/renderD130", Type: "c", Major: 226, Minor: 130},
		{Path: "/dev/dri/renderD131", Major: 226, Minor: -1},
	}, nodes)
}

func TestReadCDISpecDirectoryPrecedence(t *testing.T) {
	host, local := t.TempDir(), t.TempDir()
	writeAMDCDISpec(t, local, "kind: local\ndevices: [{name: local}]\n")

	// A missing host file permits the host-installed directory to be used.
	spec, err := ReadCDISpec([]string{host, local}, "claim.yaml")
	require.NoError(t, err)
	require.Equal(t, "local", spec.Kind)

	writeAMDCDISpec(t, host, "kind: host\ndevices: [{name: host}]\n")
	spec, err = ReadCDISpec([]string{host, local}, "claim.yaml")
	require.NoError(t, err)
	require.Equal(t, "host", spec.Kind)
	_, ok := spec.DeviceNodes("local")
	require.False(t, ok, "missing entries must not fall back to another directory")

	writeAMDCDISpec(t, host, "devices: [")
	spec, err = ReadCDISpec([]string{host, local}, "claim.yaml")
	require.Error(t, err)
	require.Nil(t, spec, "malformed host specs must not fall back to stale local specs")

	// All entries are decoded, not just whichever entry a consumer requests.
	writeAMDCDISpec(t, host, `devices:
- name: good
- name: bad
  containerEdits:
    deviceNodes:
    - path: /dev/kfd
      minor: invalid
`)
	spec, err = ReadCDISpec([]string{host, local}, "claim.yaml")
	require.Error(t, err)
	require.Nil(t, spec)
}

func TestReadCDISpecFilenameBoundary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "specs")
	require.NoError(t, os.Mkdir(dir, 0o700))
	writeAMDCDISpec(t, root, "devices: [{name: outside}]\n")
	for _, filename := range []string{
		"", ".", "..", "../claim.yaml", `..\claim.yaml`, "sub/claim.yaml",
		`sub\claim.yaml`, filepath.Join(root, "claim.yaml"),
	} {
		t.Run(filename, func(t *testing.T) {
			spec, err := ReadCDISpec([]string{dir}, filename)
			require.Error(t, err)
			require.Nil(t, spec)
		})
	}
	for _, dirs := range [][]string{nil, {dir}} {
		spec, err := ReadCDISpec(dirs, "claim.yaml")
		require.ErrorIs(t, err, os.ErrNotExist)
		require.Nil(t, spec)
	}
}
