package tune

import (
	"fmt"
	"strings"
)

// Oversubscription (RFC 7.5): these functions turn the server's facts and
// each cluster's claim into the budget sheet, the warnings, and the
// refusals: memory_overcommitted (a new or larger size past the budget,
// unless the user allows it) and would_not_start.

// Warning codes. They are stable for agents.
const (
	WarnMemoryOvercommitted = "memory_overcommitted"
	WarnCPUCrowded          = "cpu_crowded"
	WarnDiskCrowded         = "disk_crowded"
	ReasonWouldNotStart     = "would_not_start"
	// ReasonMemoryOvercommitted is the refusal's reason: the same code as
	// the warning --allow-overcommit turns it into.
	ReasonMemoryOvercommitted = WarnMemoryOvercommitted
)

// Thresholds from RFC 7.5 and 7.7.
const (
	// CrowdedClustersPerVCPU is how many clusters per vCPU are crowded.
	CrowdedClustersPerVCPU = 2
	// DiskCrowdedPercent is the share of the data disk that is crowded.
	DiskCrowdedPercent = 80
	// ArchiveSpoolGiB is pgBackRest's archive-push-queue-max, in total.
	ArchiveSpoolGiB = 4
)

// Demand is one cluster's claim on the server.
type Demand struct {
	Name      string `json:"name"`
	Size      string `json:"size"`
	MemoryMiB int    `json:"memory_mib"`
	CPUWeight int    `json:"cpu_weight"`
	// CPUQuotaPercent is cpu_quota as a percentage of one vCPU; 0 is unset.
	CPUQuotaPercent int `json:"cpu_quota_percent,omitempty"`
	// SharedMemoryMiB is what the postmaster allocates at start-up.
	SharedMemoryMiB int `json:"shared_memory_mib"`
	// HugePages is the cluster's share of the pool; 0 with huge_pages off.
	HugePages int `json:"huge_pages"`
	// HugePagesRequired is huge_pages = on: start-up fails without them.
	HugePagesRequired bool `json:"huge_pages_required,omitempty"`
	MaxWALGiB         int  `json:"max_wal_gib"`
	Running           bool `json:"running"`
}

// Host is the server's side of the arithmetic. MemoryKnown is false when
// /proc/meminfo couldn't be read; nothing is refused then.
type Host struct {
	RAMMiB          int
	VCPU            int
	ReserveMiB      int
	MemoryKnown     bool
	CommitLimitMiB  int
	CommittedMiB    int
	AvailableMiB    int
	HugePagesFree   int
	HugePageSizeKiB int
	DiskGiB         int
	DiskUsedGiB     int
}

// Sheet is the server's budget sheet (RFC 7.5), the one tune show prints.
type Sheet struct {
	RAMMiB       int      `json:"ram_mib"`
	VCPU         int      `json:"vcpu"`
	ReserveMiB   int      `json:"reserve_mib"`
	BudgetMiB    int      `json:"cluster_budget_mib"`
	AllocatedMiB int      `json:"allocated_mib"`
	FreeMiB      int      `json:"free_mib"`
	Clusters     []Demand `json:"clusters"`
}

// Warning is one oversubscription warning.
type Warning struct {
	Code    string
	Message string
}

// SharedMemoryMiB is the shared memory a cluster with sharedBuffersMiB
// allocates at start-up: the same estimate as its huge pages (RFC 7.3),
// shared_buffers plus 5% and 128 MiB for lock tables and slots.
func SharedMemoryMiB(sharedBuffersMiB int) int {
	return HugePages(sharedBuffersMiB) * 2
}

// NewSheet adds up what the clusters ask for against the cluster budget.
func NewSheet(h Host, clusters []Demand) Sheet {
	s := Sheet{RAMMiB: h.RAMMiB, VCPU: h.VCPU, ReserveMiB: h.ReserveMiB, BudgetMiB: ClusterBudget(h.RAMMiB, h.ReserveMiB), Clusters: clusters}
	for _, c := range clusters {
		s.AllocatedMiB += c.MemoryMiB
	}
	s.FreeMiB = s.BudgetMiB - s.AllocatedMiB
	return s
}

// Warnings are the sheet's memory_overcommitted, cpu_crowded and
// disk_crowded warnings. db create, db resize and db fork refuse a change
// that overcommits (see the node's checkBudget) unless it is allowed; the
// warnings then describe the server as it is.
func (s Sheet) Warnings(h Host) []Warning {
	var out []Warning
	if s.FreeMiB < 0 {
		out = append(out, Warning{Code: WarnMemoryOvercommitted, Message: fmt.Sprintf(
			"%s of database sizes on a %s budget (%s RAM less a %s reserve): under load they are throttled and the page cache shrinks. Shrink one with vaire db resize, move one to its own node, or use a bigger server.",
			FormatMiB(s.AllocatedMiB), FormatMiB(s.BudgetMiB), FormatMiB(s.RAMMiB), FormatMiB(s.ReserveMiB))})
	}
	quota := 0
	for _, c := range s.Clusters {
		quota += c.CPUQuotaPercent
	}
	switch {
	case h.VCPU > 0 && len(s.Clusters) > CrowdedClustersPerVCPU*h.VCPU:
		out = append(out, Warning{Code: WarnCPUCrowded, Message: fmt.Sprintf(
			"%d databases on %d vCPU: more than %d per vCPU, so each gets little CPU under contention", len(s.Clusters), h.VCPU, CrowdedClustersPerVCPU)})
	case h.VCPU > 0 && quota > 100*h.VCPU:
		out = append(out, Warning{Code: WarnCPUCrowded, Message: fmt.Sprintf(
			"the cpu_quota values add up to %d%% on %d vCPU (%d%%)", quota, h.VCPU, 100*h.VCPU)})
	}
	if h.DiskGiB > 0 {
		wal := 0
		for _, c := range s.Clusters {
			wal += c.MaxWALGiB
		}
		need := h.DiskUsedGiB + wal + ArchiveSpoolGiB
		if need*100 > h.DiskGiB*DiskCrowdedPercent {
			out = append(out, Warning{Code: WarnDiskCrowded, Message: fmt.Sprintf(
				"%d GiB used, %d GiB of max_wal_size and a %d GiB archive spool is over %d%% of the %d GiB data disk",
				h.DiskUsedGiB, wal, ArchiveSpoolGiB, DiskCrowdedPercent, h.DiskGiB)})
		}
	}
	return out
}

// WouldNotStart says why c's postmaster would fail to start (RFC 7.5): its
// shared memory doesn't fit in what is left of CommitLimit, or it requires
// huge pages the pool can't supply. freedMiB is what stopping it first
// gives back (its old shared memory, for a restart). The reason is empty
// when it starts.
func WouldNotStart(h Host, c Demand, freedMiB int) string {
	if !h.MemoryKnown {
		return ""
	}
	pageMiB := h.HugePageSizeKiB / 1024
	if pageMiB <= 0 {
		pageMiB = 2
	}
	if c.HugePages > 0 && h.HugePagesFree >= c.HugePages {
		return ""
	}
	if c.HugePagesRequired {
		grow := h.AvailableMiB / pageMiB
		if h.HugePagesFree+grow < c.HugePages {
			return fmt.Sprintf("%s requires %d huge pages, but the pool has %d free and %s of memory can grow it by about %d",
				c.Name, c.HugePages, h.HugePagesFree, FormatMiB(h.AvailableMiB), grow)
		}
		return ""
	}
	left := h.CommitLimitMiB - h.CommittedMiB + freedMiB
	if c.SharedMemoryMiB > left {
		return fmt.Sprintf("%s needs %s of shared memory at start-up, but only %s of CommitLimit is left (%s limit, %s committed)",
			c.Name, FormatMiB(c.SharedMemoryMiB), FormatMiB(maxInt(0, left)), FormatMiB(h.CommitLimitMiB), FormatMiB(h.CommittedMiB))
	}
	return ""
}

// SmallestThatStarts is the first of candidates (smallest first) that
// WouldNotStart lets through, or "" when none does.
func SmallestThatStarts(h Host, candidates []Demand, freedMiB int) string {
	for _, c := range candidates {
		if WouldNotStart(h, c, freedMiB) == "" {
			return c.Size
		}
	}
	return ""
}

// LargestThatFits is the largest of candidates (smallest first) whose
// memory fits in freeMiB and is above aboveMiB, or "" when none does. db
// create and db fork pass 0; db resize passes the database's current memory,
// so only a larger size counts.
func LargestThatFits(candidates []Demand, freeMiB, aboveMiB int) string {
	picked := ""
	for _, c := range candidates {
		if c.MemoryMiB <= freeMiB && c.MemoryMiB > aboveMiB {
			picked = c.Size
		}
	}
	return picked
}

// FormatMiB prints mib in the largest binary unit that keeps it readable:
// 512 MiB, 1 GiB, 4.5 GiB.
func FormatMiB(mib int) string {
	if mib < 0 {
		return "-" + FormatMiB(-mib)
	}
	if mib < 1024 {
		return fmt.Sprintf("%d MiB", mib)
	}
	s := fmt.Sprintf("%.1f", float64(mib)/1024)
	return strings.TrimSuffix(s, ".0") + " GiB"
}
