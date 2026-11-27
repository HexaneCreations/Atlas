import { useState } from "react";
import { useSearchParams } from "react-router";
import { FolderTree, Globe } from "lucide-react";
import { useMonitors } from "../api/queries";
import { emptyArray } from "../api/empty";
import type { Monitor, MonitorGroup } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { EmptyAction, EmptyState } from "../components/EmptyState";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { emptyArt } from "../lib/assets";
import { GroupManager } from "./websites/GroupManager";
import { MonitorDetail } from "./websites/MonitorDetail";
import { MonitorFormModal } from "./websites/MonitorForm";
import { MonitorList } from "./websites/MonitorList";
import { NO_FILTERS, statusCounts, type MonitorFilters } from "./websites/monitorModel";

/**
 * Website monitors: the list on the left, the selected monitor on the
 * right (below it on narrow screens). Reached with the websites page
 * grant; writing needs notifications.manage, which the server checks on
 * every request regardless of what is shown here.
 */
export function WebsitesPage() {
  const { user } = useAuth();
  const canManage = user?.can_manage_notifications ?? false;
  const query = useMonitors();
  const [params, setParams] = useSearchParams();
  const [filters, setFilters] = useState<MonitorFilters>(NO_FILTERS);
  const [form, setForm] = useState<{ monitor: Monitor | null } | null>(null);
  const [groupsOpen, setGroupsOpen] = useState(false);

  const monitors = query.data?.monitors ?? emptyArray<Monitor>();
  const groups = query.data?.groups ?? emptyArray<MonitorGroup>();
  const probe = query.data?.probe;
  const requested = params.get("monitor");
  const selectedID = requested && monitors.some((m) => m.id === requested) ? requested : (monitors[0]?.id ?? null);
  const select = (id: string | null) => {
    setParams(id ? { monitor: id } : {}, { replace: true });
  };
  const counts = statusCounts(monitors);
  const add = () => { setForm({ monitor: null }); };

  return (
    <>
      <PageHeader
        {...(canManage
          ? {
              action: (
                <div className="flex flex-wrap gap-2">
                  <Button icon={FolderTree} onClick={() => { setGroupsOpen(true); }}>Groups</Button>
                  <Button variant="primary" icon={Globe} onClick={add}>Add website</Button>
                </div>
              ),
            }
          : {})}
        {...(monitors.length > 0
          ? {
              stats: [
                { label: "Up", value: String(counts.up), tone: "success" as const },
                { label: "Down", value: String(counts.down), tone: counts.down > 0 ? ("danger" as const) : ("default" as const) },
                { label: "Paused", value: String(counts.paused) },
              ],
            }
          : {})}
      />

      {query.data && monitors.length === 0 ? (
        <Card level="floating" className="p-0">
          <EmptyState
            art={emptyArt.data}
            title="No websites yet"
            description="Add a website to check that it answers, how fast, and when its certificate expires."
            {...(canManage
              ? { action: <EmptyAction onClick={add}>Add website</EmptyAction> }
              : { hint: "Someone who can manage notifications can add one." })}
          />
        </Card>
      ) : !query.data ? (
        <Card level="floating">
          <QueryState isPending={query.isPending} error={query.error} onRetry={() => void query.refetch()} rows={6} />
        </Card>
      ) : (
        <div className="grid gap-6 lg:grid-cols-[minmax(18rem,24rem)_1fr]">
          <Card level="floating" className="self-start">
            <MonitorList
              monitors={monitors}
              groups={groups}
              filters={filters}
              onFilters={setFilters}
              selectedID={selectedID}
              onSelect={select}
            />
          </Card>
          {selectedID ? (
            <MonitorDetail
              key={selectedID}
              monitorID={selectedID}
              probe={probe}
              groups={groups}
              canManage={canManage}
              onEdit={(m) => { setForm({ monitor: m }); }}
              onDeleted={() => { select(null); }}
            />
          ) : null}
        </div>
      )}

      {form ? (
        <MonitorFormModal
          monitor={form.monitor}
          groups={groups}
          onClose={() => { setForm(null); }}
          onSaved={(saved) => { select(saved.id); }}
        />
      ) : null}
      {groupsOpen ? <GroupManager groups={groups} monitors={monitors} onClose={() => { setGroupsOpen(false); }} /> : null}
    </>
  );
}
