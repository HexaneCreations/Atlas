import { useState } from "react";
import { Check, Pencil, Plus, Trash2, X } from "lucide-react";
import { useDeleteMonitorGroup, useSaveMonitorGroup } from "../../api/queries";
import type { Monitor, MonitorGroup } from "../../api/types";
import { Button } from "../../components/Button";
import { Modal, ModalActions } from "../../components/Modal";
import { messageFor } from "../../lib/apiMessage";
import { INPUT } from "../notifications/ChannelForm";
import { RULES } from "./monitorModel";

/** Creates, renames and deletes groups. A monitor joins a group from its
 *  own form. */
export function GroupManager({
  groups,
  monitors,
  onClose,
}: {
  groups: readonly MonitorGroup[];
  monitors: readonly Monitor[];
  onClose: () => void;
}) {
  const save = useSaveMonitorGroup();
  const remove = useDeleteMonitorGroup();
  const [name, setName] = useState("");
  const [editing, setEditing] = useState<{ id: string; name: string } | null>(null);
  const [deleting, setDeleting] = useState<MonitorGroup | null>(null);
  const [error, setError] = useState<string | null>(null);
  const sorted = [...groups].sort((a, b) => a.position - b.position || a.name.localeCompare(b.name));
  const count = (id: string) => monitors.filter((m) => m.group_id === id).length;

  function create() {
    const trimmed = name.trim();
    if (trimmed === "" || trimmed.length > RULES.maxName) {
      setError(`Enter a group name of at most ${RULES.maxName} characters.`);
      return;
    }
    setError(null);
    save.mutate(
      { groupID: undefined, body: { name: trimmed, position: sorted.length } },
      { onSuccess: () => { setName(""); }, onError: (e) => { setError(messageFor(e)); } },
    );
  }

  function rename() {
    if (!editing) return;
    const trimmed = editing.name.trim();
    if (trimmed === "" || trimmed.length > RULES.maxName) {
      setError(`Enter a group name of at most ${RULES.maxName} characters.`);
      return;
    }
    setError(null);
    save.mutate(
      { groupID: editing.id, body: { name: trimmed } },
      { onSuccess: () => { setEditing(null); }, onError: (e) => { setError(messageFor(e)); } },
    );
  }

  return (
    <>
      <Modal title="Groups" onClose={onClose} width="md">
        {sorted.length === 0 ? (
          <p className="text-sm text-text-muted">No groups yet. Groups collect related websites in the list.</p>
        ) : (
          <ul className="flex flex-col divide-y divide-border">
            {sorted.map((g) => (
              <li key={g.id} className="flex items-center gap-2 py-2">
                {editing?.id === g.id ? (
                  <form
                    className="flex flex-1 items-center gap-2"
                    onSubmit={(e) => { e.preventDefault(); rename(); }}
                  >
                    <input
                      autoFocus
                      aria-label={`New name for ${g.name}`}
                      value={editing.name}
                      maxLength={RULES.maxName}
                      onChange={(e) => { setEditing({ id: g.id, name: e.target.value }); }}
                      className={`${INPUT} flex-1`}
                    />
                    <Button size="sm" type="submit" icon={Check} aria-label={`Save the name of ${g.name}`} disabled={save.isPending}>Save</Button>
                    <Button size="sm" type="button" variant="ghost" icon={X} aria-label="Cancel renaming" onClick={() => { setEditing(null); }}>Cancel</Button>
                  </form>
                ) : (
                  <>
                    <span className="min-w-0 flex-1 truncate text-sm text-text">{g.name}</span>
                    <span className="text-xs text-text-muted">{count(g.id)} websites</span>
                    <Button size="sm" variant="ghost" icon={Pencil} aria-label={`Rename ${g.name}`}
                      onClick={() => { setError(null); setEditing({ id: g.id, name: g.name }); }}>Rename</Button>
                    <Button size="sm" variant="ghost" icon={Trash2} aria-label={`Delete ${g.name}`}
                      onClick={() => { setError(null); setDeleting(g); }}>Delete</Button>
                  </>
                )}
              </li>
            ))}
          </ul>
        )}
        <form
          className="mt-4 flex items-center gap-2 border-t border-border pt-4"
          onSubmit={(e) => { e.preventDefault(); create(); }}
        >
          <input
            aria-label="New group name"
            placeholder="New group name"
            value={name}
            maxLength={RULES.maxName}
            onChange={(e) => { setName(e.target.value); }}
            className={`${INPUT} flex-1`}
          />
          <Button type="submit" size="sm" icon={Plus} disabled={save.isPending}>Add group</Button>
        </form>
        <p aria-live="polite" className="mt-2 text-sm text-danger">{error}</p>
        <ModalActions>
          <Button type="button" variant="secondary" onClick={onClose}>Done</Button>
        </ModalActions>
      </Modal>
      {deleting ? (
        <Modal title="Delete group" onClose={() => { setDeleting(null); }}>
          <p className="text-sm text-text-muted">
            Delete <span className="font-medium text-text">{deleting.name}</span>? Its {count(deleting.id)} websites are kept,
            in no group.
          </p>
          <ModalActions>
            <Button type="button" variant="secondary" autoFocus onClick={() => { setDeleting(null); }}>Cancel</Button>
            <Button
              type="button"
              variant="danger"
              disabled={remove.isPending}
              onClick={() => {
                remove.mutate(deleting.id, {
                  onSuccess: () => { setDeleting(null); },
                  onError: (e) => { setError(messageFor(e)); setDeleting(null); },
                });
              }}
            >
              {remove.isPending ? "Deleting…" : "Delete"}
            </Button>
          </ModalActions>
        </Modal>
      ) : null}
    </>
  );
}
