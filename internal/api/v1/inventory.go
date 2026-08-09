package v1

import (
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/plugin/cron"
	"github.com/hexane/atlas/internal/plugin/ports"
	"github.com/hexane/atlas/internal/plugin/process"
	"github.com/hexane/atlas/internal/plugin/service"
	"github.com/hexane/atlas/internal/plugin/system"
)

// These endpoints serve inventory rather than time series.
//
// "What is running right now", "which units are failing", "what is scheduled"
// are questions about current state. Storing them as samples would cost a
// series per process or per job — unbounded in the first case — and would
// still answer up to an interval out of date.

// ProcessResponse is one process.
type ProcessResponse struct {
	PID           int32   `json:"pid"`
	PPID          int32   `json:"ppid"`
	Name          string  `json:"name"`
	Executable    string  `json:"executable,omitempty"`
	Cmdline       string  `json:"cmdline,omitempty"`
	Username      string  `json:"username,omitempty"`
	State         string  `json:"state"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryRSS     uint64  `json:"memory_rss"`
	MemoryPercent float64 `json:"memory_percent"`
	Threads       int32   `json:"threads"`
	RunningFor    float64 `json:"running_for_seconds,omitempty"`
}

// ListProcessesResponse is the process inventory.
type ListProcessesResponse struct {
	// NodeID names the host this inventory describes.
	NodeID    string            `json:"node_id"`
	Processes []ProcessResponse `json:"processes"`
	Total     int               `json:"total"`
	// ObservedAt is when this inventory was actually read on the host. Equal
	// to "now" for a live local read; up to a push interval old for a remote
	// one — see Live.
	ObservedAt time.Time `json:"observed_at"`
	// Live is true only when read directly from the host this instant.
	// False means "as of ObservedAt", however recent that happens to be.
	// Presenting a snapshot as current is exactly the class of mistake this
	// field exists to prevent — see
	// docs/architecture/agent-design.md sec 5.
	Live bool `json:"live"`
}

// ListProcesses returns the heaviest processes on this host.
func (h *Handler) ListProcesses(w http.ResponseWriter, r *http.Request) error {
	procs, meta, err := resolveInventory(h, r, "process", inventory.SubjectProcesses, h.deps.Collection.Processes)
	if err != nil {
		return err
	}

	out := make([]ProcessResponse, 0, len(procs))
	for _, p := range procs {
		out = append(out, ProcessResponse{
			PID: p.PID, PPID: p.PPID, Name: p.Name,
			Executable: p.Executable, Cmdline: p.Cmdline, Username: p.Username,
			State: string(p.State), CPUPercent: p.CPUPercent,
			MemoryRSS: p.MemoryRSS, MemoryPercent: p.MemoryPercent,
			Threads: p.Threads, RunningFor: p.RunningFor().Seconds(),
		})
	}

	httpx.JSON(w, r, http.StatusOK, ListProcessesResponse{
		NodeID: meta.NodeID, Processes: out, Total: len(out),
		ObservedAt: meta.ObservedAt, Live: meta.Live,
	})
	return nil
}

// ServiceResponse is one systemd unit.
type ServiceResponse struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	ActiveState string    `json:"active_state"`
	SubState    string    `json:"sub_state"`
	LoadState   string    `json:"load_state,omitempty"`
	Enabled     bool      `json:"enabled"`
	Failed      bool      `json:"failed"`
	Running     bool      `json:"running"`
	MainPID     uint32    `json:"main_pid,omitempty"`
	Restarts    uint32    `json:"restart_count"`
	ActiveSince time.Time `json:"active_since,omitzero"`
	Uptime      float64   `json:"uptime_seconds,omitempty"`
	MemoryBytes uint64    `json:"memory_bytes,omitempty"`
}

// ListServicesResponse is the service inventory.
type ListServicesResponse struct {
	NodeID     string            `json:"node_id"`
	Services   []ServiceResponse `json:"services"`
	Total      int               `json:"total"`
	Failed     int               `json:"failed"`
	ObservedAt time.Time         `json:"observed_at"`
	Live       bool              `json:"live"`
}

// ListServices returns every systemd unit, failed ones first.
func (h *Handler) ListServices(w http.ResponseWriter, r *http.Request) error {
	units, meta, err := resolveInventory(h, r, "service", inventory.SubjectServices, h.deps.Collection.Services)
	if err != nil {
		return err
	}

	out := make([]ServiceResponse, 0, len(units))
	failed := 0
	for _, u := range units {
		if u.Failed() {
			failed++
		}
		out = append(out, ServiceResponse{
			Name: u.Name, Description: u.Description,
			ActiveState: string(u.ActiveState), SubState: string(u.SubState),
			LoadState: u.LoadState, Enabled: u.Enabled,
			Failed: u.Failed(), Running: u.Running(),
			MainPID: u.MainPID, Restarts: u.RestartCount,
			ActiveSince: u.ActiveSince, Uptime: u.Uptime().Seconds(),
			MemoryBytes: u.MemoryBytes,
		})
	}

	httpx.JSON(w, r, http.StatusOK, ListServicesResponse{
		NodeID: meta.NodeID, Services: out, Total: len(out), Failed: failed,
		ObservedAt: meta.ObservedAt, Live: meta.Live,
	})
	return nil
}

// CronJobResponse is one scheduled job.
type CronJobResponse struct {
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	User     string `json:"user,omitempty"`
	Source   string `json:"source"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Root     bool   `json:"root"`
}

// ListCronJobsResponse is the scheduled-job inventory.
type ListCronJobsResponse struct {
	NodeID     string            `json:"node_id"`
	Jobs       []CronJobResponse `json:"jobs"`
	Total      int               `json:"total"`
	Root       int               `json:"root"`
	ObservedAt time.Time         `json:"observed_at"`
	Live       bool              `json:"live"`
}

// ListCronJobs returns every readable scheduled job.
func (h *Handler) ListCronJobs(w http.ResponseWriter, r *http.Request) error {
	jobs, meta, err := resolveInventory(h, r, "cron", inventory.SubjectCronJobs, h.deps.Collection.CronJobs)
	if err != nil {
		return err
	}

	out := make([]CronJobResponse, 0, len(jobs))
	root := 0
	for _, j := range jobs {
		if j.Root() {
			root++
		}
		out = append(out, CronJobResponse{
			Schedule: j.Schedule, Command: j.Command, User: j.User,
			Source: string(j.Source), File: j.File, Line: j.Line, Root: j.Root(),
		})
	}

	httpx.JSON(w, r, http.StatusOK, ListCronJobsResponse{
		NodeID: meta.NodeID, Jobs: out, Total: len(out), Root: root,
		ObservedAt: meta.ObservedAt, Live: meta.Live,
	})
	return nil
}

// CertificateResponse is what a TLS probe found on a port.
type CertificateResponse struct {
	Subject         string    `json:"subject,omitempty"`
	Issuer          string    `json:"issuer,omitempty"`
	SANs            []string  `json:"sans,omitempty"`
	NotBefore       time.Time `json:"not_before,omitzero"`
	NotAfter        time.Time `json:"not_after,omitzero"`
	DaysUntilExpiry int       `json:"days_until_expiry"`
	Expired         bool      `json:"expired"`
	SelfSigned      bool      `json:"self_signed"`
}

// PortResponse is one listening socket.
type PortResponse struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     uint32 `json:"port"`
	// PID and Process are absent, not zero/empty, when Atlas could not
	// resolve the socket's owner — most often a permission boundary, not a
	// failure. See the ports package doc for why that is normal.
	PID     int32  `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`

	// TLSProbed reports whether the last probe cycle attempted a TLS handshake
	// on this port. It disambiguates an absent TLS field, which otherwise means
	// either "this service is plaintext" or "Atlas has not looked" — probing is
	// budgeted and TCP-only, so both happen routinely. A security view that
	// conflates them reports an absence of evidence as evidence of absence.
	TLSProbed bool                 `json:"tls_probed"`
	TLS       *CertificateResponse `json:"tls,omitempty"`
}

// ListPortsResponse is the listening-port inventory.
type ListPortsResponse struct {
	NodeID     string         `json:"node_id"`
	Ports      []PortResponse `json:"ports"`
	Total      int            `json:"total"`
	ObservedAt time.Time      `json:"observed_at"`
	Live       bool           `json:"live"`
}

// ListPorts returns every listening port on this host, with certificate
// detail for the ones found speaking TLS.
func (h *Handler) ListPorts(w http.ResponseWriter, r *http.Request) error {
	listeners, meta, err := resolveInventory(h, r, "ports", inventory.SubjectPorts, h.deps.Collection.Ports)
	if err != nil {
		return err
	}

	out := make([]PortResponse, 0, len(listeners))
	for _, l := range listeners {
		resp := PortResponse{
			Protocol: string(l.Socket.Protocol), Address: l.Socket.Address, Port: l.Socket.Port,
			PID: l.Socket.PID, Process: l.Socket.Process, TLSProbed: l.TLSProbed,
		}
		if l.TLS != nil {
			resp.TLS = &CertificateResponse{
				Subject: l.TLS.Subject, Issuer: l.TLS.Issuer, SANs: l.TLS.SANs,
				NotBefore: l.TLS.NotBefore, NotAfter: l.TLS.NotAfter,
				DaysUntilExpiry: l.TLS.DaysUntilExpiry(),
				Expired:         l.TLS.Expired(),
				SelfSigned:      l.TLS.SelfSigned,
			}
		}
		out = append(out, resp)
	}

	httpx.JSON(w, r, http.StatusOK, ListPortsResponse{
		NodeID: meta.NodeID, Ports: out, Total: len(out),
		ObservedAt: meta.ObservedAt, Live: meta.Live,
	})
	return nil
}

// MountResponse is one mounted filesystem.
type MountResponse struct {
	Device            string  `json:"device"`
	Mountpoint        string  `json:"mountpoint"`
	Fstype            string  `json:"fstype"`
	Total             uint64  `json:"total"`
	Used              uint64  `json:"used"`
	Free              uint64  `json:"free"`
	UsedPercent       float64 `json:"used_percent"`
	InodesTotal       uint64  `json:"inodes_total,omitempty"`
	InodesUsedPercent float64 `json:"inodes_used_percent,omitempty"`
}

// ListMountsResponse is the mounted-filesystem inventory.
type ListMountsResponse struct {
	NodeID     string          `json:"node_id"`
	Mounts     []MountResponse `json:"mounts"`
	Total      int             `json:"total"`
	ObservedAt time.Time       `json:"observed_at"`
	Live       bool            `json:"live"`
}

// ListMounts returns every mounted filesystem worth reporting on this host.
func (h *Handler) ListMounts(w http.ResponseWriter, r *http.Request) error {
	mounts, meta, err := resolveInventory(h, r, "system", inventory.SubjectMounts, h.deps.Collection.Mounts)
	if err != nil {
		return err
	}

	out := make([]MountResponse, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, MountResponse{
			Device: m.Device, Mountpoint: m.Mountpoint, Fstype: m.Fstype,
			Total: m.Total, Used: m.Used, Free: m.Free, UsedPercent: m.UsedPercent,
			InodesTotal: m.InodesTotal, InodesUsedPercent: m.InodesUsedPercent,
		})
	}

	httpx.JSON(w, r, http.StatusOK, ListMountsResponse{
		NodeID: meta.NodeID, Mounts: out, Total: len(out),
		ObservedAt: meta.ObservedAt, Live: meta.Live,
	})
	return nil
}

// requirePlugin fails with a typed error when a plugin is not active here.
//
// `not_implemented` rather than `not_found`: the endpoint exists in the API
// surface, but this host has no systemd or no readable crontabs. That is a
// permanent property of the host, not something a retry or a different path
// would fix — and it is what lets the UI say "not available here" rather than
// showing an empty list that reads as "nothing is scheduled".
func (h *Handler) requirePlugin(id string) error {
	const op = "v1.Handler.requirePlugin"

	if h.deps.Collection == nil {
		return errs.New(errs.CodeUnavailable, "collection is not configured").WithOp(op)
	}
	if !h.deps.Collection.PluginActive(id) {
		return errs.New(errs.CodeNotImplemented,
			"the %s integration is not available on this host", id).
			WithOp(op).WithDetail("plugin", id)
	}
	return nil
}

// Scope-before-plugin precedence — a caller asking about a remote node is
// not asking whether this host has systemd, and answering "no service
// manager on this host" to that question describes the wrong machine — now
// lives in [resolveInventory], which also handles the remote case rather than
// refusing it outright. This function used to do only the refusal half; it
// was superseded when remote inventory (docs/architecture/agent-design.md
// §5) was implemented and has been removed.

// Compile-time proof the inventory types are reachable from this package.
var (
	_ = process.Process{}
	_ = service.Unit{}
	_ = cron.Job{}
	_ = ports.Listener{}
	_ = system.MountInfo{}
)
