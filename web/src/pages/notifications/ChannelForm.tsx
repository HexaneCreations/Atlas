import { useEffect, useState, type ReactNode } from "react";
import { Send, Trash2 } from "lucide-react";
import { testNotificationDraft } from "../../api/client";
import { useDeleteNotificationChannel, useSaveNotificationChannel } from "../../api/queries";
import type { NotificationChannel, NotificationChannelType, NotificationSeverity } from "../../api/types";
import { Button } from "../../components/Button";
import { Modal, ModalActions } from "../../components/Modal";
import { useToast } from "../../components/Toast";
import { messageFor } from "../../lib/apiMessage";
import {
  TYPE_LABEL,
  canKeepURL,
  runTest,
  secondsLeft,
  secretUnreadable,
  serverErrors,
  toDraft,
  toRequest,
  validateDraft,
  type ChannelDraft,
  type ChannelErrors,
  type ChannelField,
  type TestResult,
} from "./channelModel";
import { DEFAULT_TEMPLATE, DENIED_HEADERS, TEAMS_HOST_SUFFIXES, TEMPLATE_VARIABLES } from "./channelRules";

const INPUT =
  "w-full rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary disabled:opacity-60";

/** Creates a channel, or edits and deletes a saved one. */
export function ChannelFormModal({
  channel,
  onClose,
}: {
  channel: NotificationChannel | null;
  onClose: () => void;
}) {
  const [draft, setDraft] = useState(() => toDraft(channel));
  const [errors, setErrors] = useState<ChannelErrors>({});
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<TestResult | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const [confirmDelete, setConfirmDelete] = useState(false);
  const save = useSaveNotificationChannel();
  const { push } = useToast();

  const wait = secondsLeft(test, now);
  useEffect(() => {
    if (wait === 0) return;
    const timer = window.setInterval(() => { setNow(Date.now()); }, 250);
    return () => { window.clearInterval(timer); };
  }, [wait]);

  function change(patch: Partial<ChannelDraft>) {
    setDraft((d) => ({ ...d, ...patch }));
    // A result or an error about what the form no longer says would mislead.
    setTest((t) => (t?.kind === "limited" ? t : null));
    setErrors({});
  }

  async function sendTest() {
    setTesting(true);
    setErrors({});
    const outcome = await runTest(draft, channel, testNotificationDraft);
    setErrors(outcome.errors);
    setTest(outcome.result);
    setNow(Date.now());
    setTesting(false);
  }

  function submit() {
    const problems = validateDraft(draft, channel);
    setErrors(problems);
    if (Object.keys(problems).length > 0) return;
    save.mutate(
      { channelID: channel?.id, body: toRequest(draft, channel) },
      {
        onSuccess: (saved) => {
          push({ tone: "success", title: channel ? `${saved.name} updated` : `${saved.name} created` });
          onClose();
        },
        onError: (e) => { setErrors(serverErrors(e)); },
      },
    );
  }

  return (
    <>
      <Modal title={channel ? "Edit notification" : "Add notification"} onClose={onClose} width="lg">
        <form
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <ChannelFields draft={draft} channel={channel} errors={errors} onChange={change} />
          <FormStatus error={errors.form} test={test} wait={wait} />
          <FormButtons
            existing={channel !== null}
            saving={save.isPending}
            testing={testing}
            wait={wait}
            onDelete={() => { setConfirmDelete(true); }}
            onTest={() => void sendTest()}
          />
        </form>
      </Modal>
      {confirmDelete && channel ? (
        <DeleteChannelModal
          channel={channel}
          onClose={() => { setConfirmDelete(false); }}
          onDeleted={onClose}
        />
      ) : null}
    </>
  );
}

export function ChannelFields({
  draft,
  channel,
  errors,
  onChange,
}: {
  draft: ChannelDraft;
  channel: NotificationChannel | null;
  errors: ChannelErrors;
  onChange: (patch: Partial<ChannelDraft>) => void;
}) {
  const keeps = canKeepURL(channel);
  return (
    <div className="flex flex-col gap-4">
      <Field id="channel-type" label="Notification type">
        <select
          id="channel-type"
          value={draft.type}
          disabled={channel !== null}
          onChange={(e) => { onChange({ type: e.target.value as NotificationChannelType }); }}
          className={INPUT}
        >
          <option value="teams">{TYPE_LABEL.teams}</option>
          <option value="webhook">{TYPE_LABEL.webhook}</option>
          <option value="email" disabled>Email (coming soon)</option>
        </select>
      </Field>

      <Field id="channel-name" label="Friendly name" error={errors.name}>
        <input
          id="channel-name"
          autoFocus
          required
          value={draft.name}
          onChange={(e) => { onChange({ name: e.target.value }); }}
          {...describe("name", errors)}
          className={INPUT}
        />
      </Field>

      {draft.type === "email" ? null : (
        <Field
          id="channel-url"
          label="Post URL"
          error={errors.url}
          help={
            <>
              {keeps ? <>Currently <code>https://{channel?.destination_host}/…</code>. Leave empty to keep the current URL. </> : null}
              {secretUnreadable(channel) ? "The stored URL and headers cannot be read with the server's encryption key. Enter them again. " : null}
              {draft.type === "teams" ? `The host must be, or end with, ${TEAMS_HOST_SUFFIXES.join(", ")}.` : "https only, to a public address."}
            </>
          }
        >
          <input
            id="channel-url"
            type="url"
            autoComplete="off"
            spellCheck={false}
            required={!keeps}
            value={draft.url}
            placeholder={keeps ? "Leave empty to keep the current URL" : "https://"}
            onChange={(e) => { onChange({ url: e.target.value }); }}
            {...describe("url", errors)}
            className={INPUT}
          />
        </Field>
      )}

      {draft.type === "webhook" ? (
        <>
          <Field
            id="channel-template"
            label="Request body"
            error={errors.template}
            action={
              <button
                type="button"
                onClick={() => { onChange({ template: DEFAULT_TEMPLATE }); }}
                className="text-xs font-medium text-primary hover:underline"
              >
                Reset to default
              </button>
            }
            help={
              <>
                Variables: {TEMPLATE_VARIABLES.join(", ")}. Each is already JSON, so add no quotes around it. Example:{" "}
                <code>{'{"text": {{.msg}}, "node": {{.nodeJSON}}}'}</code>
              </>
            }
          >
            <textarea
              id="channel-template"
              rows={6}
              spellCheck={false}
              value={draft.template}
              onChange={(e) => { onChange({ template: e.target.value }); }}
              {...describe("template", errors)}
              className={`${INPUT} font-mono`}
            />
          </Field>

          <Field
            id="channel-headers"
            label="Additional headers"
            error={errors.headers}
            help={
              <>
                A JSON object of names and values. Not allowed: {DENIED_HEADERS.join(", ")}.
                {channel && channel.header_names.length > 0
                  ? " Stored values are never shown. Leave a value empty to keep it, or remove the header to drop it. A new URL needs every value entered again."
                  : null}
              </>
            }
          >
            <textarea
              id="channel-headers"
              rows={4}
              autoComplete="off"
              spellCheck={false}
              value={draft.headers}
              placeholder={'{"X-Api-Key": "value"}'}
              onChange={(e) => { onChange({ headers: e.target.value }); }}
              {...describe("headers", errors)}
              className={`${INPUT} font-mono`}
            />
          </Field>
        </>
      ) : null}

      <div>
        <label className="flex items-center gap-2 text-sm text-text">
          <input
            type="checkbox"
            checked={draft.enabled}
            aria-describedby="channel-enabled-help"
            onChange={(e) => { onChange({ enabled: e.target.checked }); }}
          />
          Enabled for all nodes
        </label>
        <p id="channel-enabled-help" className="mt-1 text-xs text-text-muted">
          When on, this channel receives alerts from every node. To silence one node, use Mute on the Nodes page.
        </p>
      </div>

      <Field id="channel-severity" label="Minimum severity">
        <select
          id="channel-severity"
          value={draft.minSeverity}
          onChange={(e) => { onChange({ minSeverity: e.target.value as NotificationSeverity }); }}
          className={INPUT}
        >
          <option value="warning">Warning</option>
          <option value="critical">Critical</option>
        </select>
      </Field>
    </div>
  );
}

function describe(field: ChannelField, errors: ChannelErrors) {
  const id = `channel-${field}`;
  return {
    "aria-invalid": errors[field] !== undefined,
    "aria-describedby": `${id}-help ${id}-error`,
  };
}

function Field({
  id,
  label,
  error,
  help,
  action,
  children,
}: {
  id: string;
  label: string;
  error?: string | undefined;
  help?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between gap-2">
        <label htmlFor={id} className="text-sm text-text">{label}</label>
        {action}
      </div>
      {children}
      {help ? <p id={`${id}-help`} className="text-xs break-words text-text-muted">{help}</p> : null}
      {/* Always present: a live region is only announced when it changes. */}
      <p id={`${id}-error`} aria-live="polite" className="text-xs text-danger">{error}</p>
    </div>
  );
}

export function FormStatus({ error, test, wait }: { error: string | undefined; test: TestResult | null; wait: number }) {
  return (
    <div aria-live="polite" className="mt-4 text-sm">
      {error ? <p className="text-danger">{error}</p> : null}
      {test?.kind === "ok" ? <p className="text-success">Test sent. The destination accepted it.</p> : null}
      {test?.kind === "failed" ? <p className="text-danger">Test failed: {test.message}</p> : null}
      {wait > 0 ? <p className="text-warning">Too many tests. Wait for the countdown on the Test button.</p> : null}
    </div>
  );
}

export function FormButtons({
  existing,
  saving,
  testing,
  wait,
  onDelete,
  onTest,
}: {
  existing: boolean;
  saving: boolean;
  testing: boolean;
  wait: number;
  onDelete: () => void;
  onTest: () => void;
}) {
  return (
    <ModalActions>
      {existing ? (
        <Button type="button" variant="danger" icon={Trash2} className="mr-auto" onClick={onDelete}>Delete</Button>
      ) : null}
      <Button type="button" variant="secondary" icon={Send} disabled={testing || wait > 0} onClick={onTest}>
        {wait > 0 ? `Test (${wait}s)` : testing ? "Testing…" : "Test"}
      </Button>
      <Button type="submit" variant="primary" disabled={saving}>{saving ? "Saving…" : "Save"}</Button>
    </ModalActions>
  );
}

function DeleteChannelModal({
  channel,
  onClose,
  onDeleted,
}: {
  channel: NotificationChannel;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const remove = useDeleteNotificationChannel();
  const { push } = useToast();
  const [error, setError] = useState<string | null>(null);
  return (
    <DeleteConfirm
      name={channel.name}
      pending={remove.isPending}
      error={error}
      onClose={onClose}
      onConfirm={() => {
        setError(null);
        remove.mutate(channel.id, {
          onSuccess: () => {
            push({ tone: "success", title: `${channel.name} deleted` });
            onDeleted();
          },
          onError: (e) => { setError(messageFor(e)); },
        });
      }}
    />
  );
}

export function DeleteConfirm({
  name,
  pending,
  error,
  onClose,
  onConfirm,
}: {
  name: string;
  pending: boolean;
  error: string | null;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Modal title="Delete notification" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Delete <span className="font-medium text-text">{name}</span>? Its delivery history is deleted with it.
      </p>
      <p aria-live="polite" className="mt-3 text-sm text-danger">{error}</p>
      <ModalActions>
        <Button type="button" variant="secondary" autoFocus onClick={onClose}>Cancel</Button>
        <Button type="button" variant="danger" disabled={pending} onClick={onConfirm}>
          {pending ? "Deleting…" : "Delete"}
        </Button>
      </ModalActions>
    </Modal>
  );
}
