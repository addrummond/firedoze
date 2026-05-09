package model

import (
	"encoding/json"
	"fmt"
)

type JSONText string

func (j JSONText) MarshalJSON() ([]byte, error) {
	if j == "" {
		return []byte("null"), nil
	}
	data := []byte(j)
	if json.Valid(data) {
		return data, nil
	}
	return json.Marshal(string(j))
}

func (j *JSONText) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*j = ""
		return nil
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSONText")
	}
	*j = JSONText(data)
	return nil
}

func (j *JSONText) Scan(value any) error {
	switch value := value.(type) {
	case nil:
		*j = ""
	case string:
		*j = JSONText(value)
	case []byte:
		*j = JSONText(value)
	default:
		return fmt.Errorf("scan JSONText from %T", value)
	}
	return nil
}

type VM struct {
	UUID                  string   `json:"uuid"`
	Name                  string   `json:"name"`
	State                 string   `json:"state"`
	PrivateIP             string   `json:"private_ip,omitempty"`
	VCPUs                 int      `json:"vcpus"`
	MemoryMinMiB          int      `json:"memory_min_mib"`
	MemoryMaxMiB          int      `json:"memory_max_mib"`
	DiskBytes             int64    `json:"disk_bytes"`
	DefaultHTTPPort       int      `json:"default_http_port"`
	IdleSleepAfterSeconds int      `json:"idle_sleep_after_seconds,omitempty"`
	LastStartedAt         string   `json:"last_started_at,omitempty"`
	LastActivityAt        string   `json:"last_activity_at,omitempty"`
	StoppedAt             string   `json:"stopped_at,omitempty"`
	ArchivedDiskPath      string   `json:"archived_disk_path,omitempty"`
	BaseImageID           string   `json:"base_image_id,omitempty"`
	KernelID              string   `json:"kernel_id,omitempty"`
	BaseImageMetadata     JSONText `json:"base_image_metadata,omitempty"`
	AutoWake              bool     `json:"auto_wake"`
	PublicHTTP            bool     `json:"public_http"`
}

type Route struct {
	Name      string `json:"name"`
	VMUUID    string `json:"vm_uuid"`
	VMName    string `json:"vm_name"`
	Port      int    `json:"port"`
	IsDefault bool   `json:"is_default"`
}

type Snapshot struct {
	Name              string   `json:"name"`
	SourceVMUUID      string   `json:"source_vm_uuid,omitempty"`
	SourceVM          string   `json:"source_vm,omitempty"`
	StatePath         string   `json:"state_path"`
	MemPath           string   `json:"mem_path"`
	DiskPath          string   `json:"disk_path"`
	BaseImageID       string   `json:"base_image_id"`
	KernelID          string   `json:"kernel_id"`
	BaseImageMetadata JSONText `json:"base_image_metadata,omitempty"`
	CreatedAt         string   `json:"created_at"`
}

type VMResourceUsage struct {
	Name               string                `json:"name"`
	State              string                `json:"state"`
	VCPUs              int                   `json:"vcpus"`
	MemoryMinMiB       int                   `json:"memory_min_mib"`
	MemoryMaxMiB       int                   `json:"memory_max_mib"`
	DiskBytes          int64                 `json:"disk_bytes"`
	DiskAllocatedBytes int64                 `json:"disk_allocated_bytes,omitempty"`
	MemoryHotplug      *MemoryHotplugUsage   `json:"memory_hotplug,omitempty"`
	GuestMemory        *GuestMemoryReport    `json:"guest_memory,omitempty"`
	Process            *ProcessResourceUsage `json:"process,omitempty"`
	Cgroup             *CgroupResourceUsage  `json:"cgroup,omitempty"`
}

type MemoryHotplugUsage struct {
	TotalMiB     int `json:"total_mib"`
	RequestedMiB int `json:"requested_mib"`
	PluggedMiB   int `json:"plugged_mib"`
	EffectiveMiB int `json:"effective_mib"`
}

type GuestMemoryReport struct {
	ReportedAt         string  `json:"reported_at,omitempty"`
	TotalMiB           int     `json:"total_mib,omitempty"`
	AvailableMiB       int     `json:"available_mib,omitempty"`
	FreeMiB            int     `json:"free_mib,omitempty"`
	BuffersMiB         int     `json:"buffers_mib,omitempty"`
	CachedMiB          int     `json:"cached_mib,omitempty"`
	SwapTotalMiB       int     `json:"swap_total_mib,omitempty"`
	SwapFreeMiB        int     `json:"swap_free_mib,omitempty"`
	RootDiskTotalBytes uint64  `json:"root_disk_total_bytes,omitempty"`
	RootDiskFreeBytes  uint64  `json:"root_disk_free_bytes,omitempty"`
	Load1              float64 `json:"load1,omitempty"`
	Load5              float64 `json:"load5,omitempty"`
	Load15             float64 `json:"load15,omitempty"`
	LastTargetMiB      int     `json:"last_target_mib,omitempty"`
}

type ProcessResourceUsage struct {
	PID         int     `json:"pid"`
	RSSBytes    uint64  `json:"rss_bytes,omitempty"`
	VMSizeBytes uint64  `json:"vm_size_bytes,omitempty"`
	CPUSeconds  float64 `json:"cpu_seconds,omitempty"`
	Threads     int     `json:"threads,omitempty"`
}

type CgroupResourceUsage struct {
	MemoryCurrentBytes  uint64  `json:"memory_current_bytes,omitempty"`
	MemoryPeakBytes     uint64  `json:"memory_peak_bytes,omitempty"`
	CPUUsageSeconds     float64 `json:"cpu_usage_seconds,omitempty"`
	CPUUserSeconds      float64 `json:"cpu_user_seconds,omitempty"`
	CPUSystemSeconds    float64 `json:"cpu_system_seconds,omitempty"`
	CPUThrottledSeconds float64 `json:"cpu_throttled_seconds,omitempty"`
	CPUThrottledEvents  uint64  `json:"cpu_throttled_events,omitempty"`
	CPUWeight           int     `json:"cpu_weight,omitempty"`
	IOReadBytes         uint64  `json:"io_read_bytes,omitempty"`
	IOWriteBytes        uint64  `json:"io_write_bytes,omitempty"`
	IOWeight            int     `json:"io_weight,omitempty"`
}
