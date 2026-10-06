// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux

package amd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultCDISpecDirs lists host-mounted and host-installed CDI directories in priority order.
// Consumers must treat this slice as read-only; overrides replace their own slice.
var DefaultCDISpecDirs = []string{"/host/var/run/cdi", "/var/run/cdi"}

// CDIDeviceNode is a device-node edit. Absent major and minor numbers are -1.
type CDIDeviceNode struct {
	Path     string `yaml:"path"`
	HostPath string `yaml:"hostPath"`
	Type     string `yaml:"type"`
	Major    int    `yaml:"major"`
	Minor    int    `yaml:"minor"`
}

// UnmarshalYAML preserves the distinction between absent numbers and explicit zero.
func (n *CDIDeviceNode) UnmarshalYAML(value *yaml.Node) error {
	type rawDeviceNode CDIDeviceNode
	*n = CDIDeviceNode{Major: -1, Minor: -1}
	return value.Decode((*rawDeviceNode)(n))
}

type cdiContainerEdits struct {
	DeviceNodes []CDIDeviceNode `yaml:"deviceNodes"`
}

type cdiDevice struct {
	Name           string            `yaml:"name"`
	ContainerEdits cdiContainerEdits `yaml:"containerEdits"`
}

// CDISpec is the typed subset of a CDI specification used for AMD allocation resolution.
// Spec-wide edits grant shared access and are deliberately not allocation identity.
type CDISpec struct {
	Kind    string      `yaml:"kind"`
	Devices []cdiDevice `yaml:"devices"`
}

// CDIClaimUID extracts the 36-character claim UID prefix of an AMD device key.
// Claim UIDs contain hyphens, so splitting at the first hyphen would truncate them.
func CDIClaimUID(deviceKey string) string {
	const uuidLen = 36
	if len(deviceKey) < uuidLen {
		return ""
	}
	uid := deviceKey[:uuidLen]
	if strings.ContainsAny(uid, `/\`) {
		return ""
	}
	return uid
}

// ReadCDISpec reads and decodes the first readable file in dirs. A malformed readable
// file is an error, not a reason to fall back to a lower-priority directory.
// filename must be a basename. The AMD consumer validates Kind.
func ReadCDISpec(dirs []string, filename string) (*CDISpec, error) {
	if filename == "" || filename == "." || filename == ".." ||
		filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
		return nil, fmt.Errorf("CDI spec filename %q is not a basename", filename)
	}

	err := error(os.ErrNotExist)
	for _, dir := range dirs {
		var data []byte
		data, err = os.ReadFile(filepath.Join(dir, filename))
		if err != nil {
			continue
		}
		var spec CDISpec
		if err := yaml.Unmarshal(data, &spec); err != nil {
			return nil, fmt.Errorf("decode CDI spec %q: %w", filepath.Join(dir, filename), err)
		}
		return &spec, nil
	}
	return nil, fmt.Errorf("read CDI spec %q: %w", filename, err)
}

// DeviceNodes selects exactly one named device entry. The returned nodes alias
// the decoded spec and must be treated as read-only. An entry with no nodes is
// present (true); an absent entry is false. Sibling and spec-wide edits are not merged.
func (s *CDISpec) DeviceNodes(deviceKey string) ([]CDIDeviceNode, bool) {
	for i := range s.Devices {
		if s.Devices[i].Name == deviceKey {
			return s.Devices[i].ContainerEdits.DeviceNodes, true
		}
	}
	return nil, false
}
