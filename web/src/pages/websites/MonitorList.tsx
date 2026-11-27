import { useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import type { Monitor, MonitorGroup, MonitorStatus } from "../../api/types";
import { Badge } from "../../components/Badge";
import { FilterSelect, SearchInput } from "../../components/Toolbar";
import { HeartbeatBar } from "./HeartbeatBar";
import {
  displayStatus,
  filterMonitors,
  formatUptime,
  groupMonitors,
  uptimeTone,
  type MonitorFilters,
} from "./monitorModel";

const DOT: Record<string, string> = {
  success: "bg-success", danger: "bg-danger", warning: "bg-warning", neutral: "bg-text-muted", info: "bg-info",
};

/** The searchable, grouped monitor list: the left of the Websites page. */
export function MonitorList({
  monitors,
  groups,
  filters,
  onFilters,
  selectedID,
  onSelect,
}: {
  monitors: readonly Monitor[];
  groups: readonly MonitorGroup[];
  filters: MonitorFilters;
  onFilters: (f: MonitorFilters) => void;
  selectedID: string | null;
  onSelect: (id: string) => void;
}) {
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const sections = groupMonitors(filterMonitors(monitors, filters), groups);
  const toggle = (key: string) => {
    setCollapsed((c) => {
      const next = new Set(c);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  return (
    <div className="flex flex-col gap-3">
      <SearchInput
        value={filters.q}
        onChange={(q) => { onFilters({ ...filters, q }); }}
        placeholder="Search name, host or tag"
      />
      <div className="flex flex-wrap gap-2">
        <FilterSelect<"all" | MonitorStatus>
          label="Status"
          value={filters.status}
          onChange={(status) => { onFilters({ ...filters, status }); }}
          options={[
            { value: "all", label: "All statuses" },
            { value: "up", label: "Up" },
            { value: "down", label: "Down" },
            { value: "pending", label: "Pending" },
            { value: "unknown", label: "Unknown" },
          ]}
        />
        <FilterSelect<MonitorFilters["active"]>
          label="Active"
          value={filters.active}
          onChange={(active) => { onFilters({ ...filters, active }); }}
          options={[
            { value: "all", label: "Active and paused" },
            { value: "active", label: "Active" },
            { value: "paused", label: "Paused" },
          ]}
        />
        {groups.length > 0 ? (
          <FilterSelect<string>
            label="Group"
            value={filters.group}
            onChange={(group) => { onFilters({ ...filters, group }); }}
            options={[
              { value: "all", label: "All groups" },
              ...groups.map((g) => ({ value: g.id, label: g.name })),
              { value: "none", label: "No group" },
            ]}
          />
        ) : null}
      </div>

      {sections.length === 0 ? (
        <p className="py-6 text-center text-sm text-text-muted">No monitor matches these filters.</p>
      ) : (
        sections.map(({ group, monitors: rows }) => {
          const key = group?.id ?? "";
          const open = !collapsed.has(key);
          const listID = `monitor-group-${key || "none"}`;
          return (
            <section key={key} aria-label={group?.name ?? "No group"}>
              {groups.length > 0 ? (
                <button
                  type="button"
                  aria-expanded={open}
                  aria-controls={listID}
                  onClick={() => { toggle(key); }}
                  className="mb-1 flex w-full items-center gap-1.5 rounded-md px-1 py-1 text-left text-xs font-semibold uppercase tracking-wider text-text-muted hover:text-text"
                >
                  {open ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
                  {group?.name ?? "No group"}
                  <span className="ml-auto font-normal normal-case tracking-normal">{rows.length}</span>
                </button>
              ) : null}
              {open ? (
                <ul id={listID} className="flex flex-col gap-1">
                  {rows.map((m) => (
                    <li key={m.id}>
                      <MonitorRow monitor={m} selected={m.id === selectedID} onSelect={onSelect} />
                    </li>
                  ))}
                </ul>
              ) : null}
            </section>
          );
        })
      )}
    </div>
  );
}

function MonitorRow({ monitor: m, selected, onSelect }: { monitor: Monitor; selected: boolean; onSelect: (id: string) => void }) {
  const status = displayStatus(m);
  return (
    <button
      type="button"
      aria-current={selected ? "true" : undefined}
      onClick={() => { onSelect(m.id); }}
      className={`flex w-full flex-col gap-2 rounded-lg border px-3 py-2.5 text-left transition-colors focus-visible:ring-2 focus-visible:ring-primary focus-visible:outline-none ${
        selected ? "border-primary bg-primary/10" : "border-border hover:bg-surface-hover"
      }`}
    >
      <span className="flex items-center gap-2">
        <span aria-hidden="true" className={`h-2 w-2 shrink-0 rounded-full ${DOT[status.tone]}`} />
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-medium text-text">{m.name}</span>
          <span className="block truncate text-xs text-text-subtle">
            <span className="sr-only">{status.label}, </span>
            {m.host}
          </span>
        </span>
        <Badge tone={uptimeTone(m.uptime_24h)}>
          <span className="sr-only">Uptime last 24 hours </span>
          {formatUptime(m.uptime_24h)}
        </Badge>
      </span>
      <HeartbeatBar beats={m.heartbeats ?? []} />
    </button>
  );
}
