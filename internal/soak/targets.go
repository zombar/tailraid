package soak

import (
	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chart"
)

// Placement targets (docs/14): a device's own store and its added disks. The driver's
// nodes are hosts; a slot on a disk belongs to the disk's host for every per-node check.

// TargetSample is one placement target in a sample, for the disk-spread chart: the chart
// package's Target, so the samples feed it without a copy.
type TargetSample = chart.Target

// targetsOf lists the report's targets, the disks of each host in host order, with host
// names; a device row is not a target (docs/14 §9).
func targetsOf(rep admin.Report) []TargetSample {
	var out []TargetSample
	add := func(n *admin.NodeReport, host string) {
		out = append(out, TargetSample{Name: n.Name, Host: host, Slots: n.Slots, Capacity: n.Capacity, Live: n.Live, Active: n.State == "active"})
	}
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if n.Kind != "device" || n.State == "removed" {
			continue
		}
		for j := range rep.Nodes {
			if d := &rep.Nodes[j]; d.Kind == "disk" && int(d.Host) == int(n.Idx) && d.State != "removed" {
				add(d, n.Name)
			}
		}
	}
	return out
}

// hasDisks reports whether any host has an added disk.
func hasDisks(rep admin.Report) bool {
	for i := range rep.Nodes {
		if rep.Nodes[i].Kind == "disk" && rep.Nodes[i].State != "removed" {
			return true
		}
	}
	return false
}

// learnHosts records which host each disk belongs to (from a status report), so a slot
// on a disk resolves to the host that serves it.
func (r *Run) learnHosts(rep admin.Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hostOf == nil {
		r.hostOf = map[int]int{}
	}
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Kind == "disk" {
			r.hostOf[int(n.Idx)] = int(n.Host)
		}
	}
}

// hostIdx is the host of a target: itself for a device, its host for a known disk.
func (r *Run) hostIdx(idx int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hostOf[idx]; ok {
		return h
	}
	return idx
}

// voterTargets is the set of targets on voter hosts: voter devices and their disks.
func voterTargets(rep admin.Report) map[int]bool {
	voters := map[int]bool{}
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Kind == "device" && n.Voter {
			voters[int(n.Idx)] = true
		}
	}
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Kind == "disk" && voters[int(n.Host)] {
			voters[int(n.Idx)] = true
		}
	}
	return voters
}

// hostSlots is the slots a host holds on its own store and its disks.
func hostSlots(rep admin.Report, host int) int {
	total := 0
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if int(n.Idx) == host || (n.Kind == "disk" && int(n.Host) == host) {
			total += n.Slots
		}
	}
	return total
}

// diskRefs lists the report's disks as NodeRefs that name their host (for FailDisk).
func diskRefs(rep admin.Report) []NodeRef {
	names := map[int]string{}
	for i := range rep.Nodes {
		names[int(rep.Nodes[i].Idx)] = rep.Nodes[i].Name
	}
	var out []NodeRef
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Kind == "disk" && n.State == "active" {
			out = append(out, NodeRef{Name: n.Name, Idx: n.Idx, Host: names[int(n.Host)]})
		}
	}
	return out
}
