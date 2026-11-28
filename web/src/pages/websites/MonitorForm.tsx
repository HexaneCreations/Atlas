import { useEffect, useState, type ReactNode } from "react";
import { Plus, Send, Trash2, X } from "lucide-react";
import { testMonitorDraft } from "../../api/client";
import { useNotificationChannels, useSaveMonitor } from "../../api/queries";
import { emptyArray } from "../../api/empty";
import type {
  Monitor, MonitorAuthType, MonitorBodyEncoding, MonitorGroup, MonitorMethod, NotificationChannel,
} from "../../api/types";
import { Button } from "../../components/Button";
import { Modal, ModalActions } from "../../components/Modal";
import { useToast } from "../../components/Toast";
import { Field, INPUT } from "../notifications/ChannelForm";
import {
  RULES,
  hasQueryString,
  selectedChannels,
  keepsCredentials,
  runMonitorTest,
  secondsLeft,
  serverErrors,
  testSummary,
  toMonitorDraft,
  toMonitorRequest,
  validateMonitorDraft,
  type HeaderDraft,
  type MonitorDraft,
  type MonitorErrors,
  type MonitorField,
  type MonitorTestResult,
} from "./monitorModel";

/** Adds a monitor, or edits a saved one. */
export function MonitorFormModal({
  monitor,
  groups,
  onClose,
  onSaved,
}: {
  monitor: Monitor | null;
  groups: readonly MonitorGroup[];
  onClose: () => void;
  onSaved?: (saved: Monitor) => void;
}) {
  const [draft, setDraft] = useState(() => toMonitorDraft(monitor));
  const [errors, setErrors] = useState<MonitorErrors>({});
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<MonitorTestResult | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const save = useSaveMonitor();
  const channelsQuery = useNotificationChannels();
  const channels = channelsQuery.data?.channels ?? emptyArray<NotificationChannel>();
  const { push } = useToast();

  const wait = secondsLeft(test, now);
  useEffect(() => {
    if (wait === 0) return;
    const timer = window.setInterval(() => { setNow(Date.now()); }, 250);
    return () => { window.clearInterval(timer); };
  }, [wait]);

  function change(patch: Partial<MonitorDraft>) {
    setDraft((d) => ({ ...d, ...patch }));
    // A result about what the form no longer says would mislead.
    setTest((t) => (t?.kind === "limited" ? t : null));
    setErrors({});
  }

  async function sendTest() {
    setTesting(true);
    setErrors({});
    const outcome = await runMonitorTest(draft, monitor, testMonitorDraft);
    setErrors(outcome.errors);
    setTest(outcome.result);
    setNow(Date.now());
    setTesting(false);
  }

  function submit() {
    const problems = validateMonitorDraft(draft, monitor);
    setErrors(problems);
    if (Object.keys(problems).length > 0) return;
    save.mutate(
      { monitorID: monitor?.id, body: toMonitorRequest(draft, monitor) },
      {
        onSuccess: (saved) => {
          push({ tone: "success", title: monitor ? `${saved.name} updated` : `${saved.name} added` });
          onSaved?.(saved);
          onClose();
        },
        onError: (e) => { setErrors(serverErrors(e)); },
      },
    );
  }

  return (
    <Modal title={monitor ? `Edit ${monitor.name}` : "Add website"} onClose={onClose} width="lg">
      <form
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <MonitorFields draft={draft} monitor={monitor} groups={groups} channels={channels}
          channelsUnavailable={channelsQuery.isError} errors={errors} onChange={change} />
        <div aria-live="polite" className="mt-4 text-sm">
          {errors.form ? <p className="text-danger">{errors.form}</p> : null}
          {Object.keys(errors).length > 0 && !errors.form ? <p className="text-danger">Fix the fields marked above.</p> : null}
          <TestOutcome test={test} wait={wait} />
        </div>
        <ModalActions>
          <Button type="button" variant="secondary" icon={Send} disabled={testing || wait > 0} onClick={() => void sendTest()}>
            {wait > 0 ? `Test now (${wait}s)` : testing ? "Testing…" : "Test now"}
          </Button>
          <Button type="submit" variant="primary" disabled={save.isPending}>{save.isPending ? "Saving…" : "Save"}</Button>
        </ModalActions>
      </form>
    </Modal>
  );
}

export function TestOutcome({ test, wait }: { test: MonitorTestResult | null; wait: number }) {
  if (wait > 0) return <p className="text-warning">Too many tests. Wait for the countdown on the Test now button.</p>;
  if (test?.kind === "failed") return <p className="text-danger">The test could not run: {test.message}</p>;
  if (test?.kind !== "result") return null;
  const up = test.result.status === "up";
  return <p className={up ? "text-success" : "text-danger"}>Test result: {testSummary(test.result)}</p>;
}

function describe(field: MonitorField, errors: MonitorErrors) {
  const id = `monitor-${field}`;
  return { id, "aria-invalid": errors[field] !== undefined, "aria-describedby": `${id}-help ${id}-error` };
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <fieldset className="flex flex-col gap-4 border-t border-border pt-4 first:border-t-0 first:pt-0">
      <legend className="mb-1 text-xs font-semibold uppercase tracking-wider text-text-muted">{title}</legend>
      {children}
    </fieldset>
  );
}

function NumberField({
  field, label, value, help, errors, min, max, onChange,
}: {
  field: MonitorField;
  label: string;
  value: string;
  help?: string;
  errors: MonitorErrors;
  min: number;
  max: number;
  onChange: (v: string) => void;
}) {
  return (
    <Field id={`monitor-${field}`} label={label} error={errors[field]} help={help}>
      <input
        type="number"
        inputMode="numeric"
        min={min}
        max={max}
        value={value}
        onChange={(e) => { onChange(e.target.value); }}
        {...describe(field, errors)}
        className={INPUT}
      />
    </Field>
  );
}

/** Credential inputs: write-only, never prefilled. */
function SecretInput({
  field, label, value, kept, errors, onChange, type = "password",
}: {
  field: MonitorField;
  label: string;
  value: string;
  kept: boolean;
  errors: MonitorErrors;
  onChange: (v: string) => void;
  type?: "password" | "text";
}) {
  return (
    <Field
      id={`monitor-${field}`}
      label={label}
      error={errors[field]}
      help={kept ? "Configured. Stored values are never shown; leave empty to keep it." : undefined}
    >
      <input
        type={type}
        autoComplete="off"
        spellCheck={false}
        value={value}
        placeholder={kept ? "Configured — leave empty to keep" : ""}
        onChange={(e) => { onChange(e.target.value); }}
        {...describe(field, errors)}
        className={INPUT}
      />
    </Field>
  );
}

export function MonitorFields({
  draft: d,
  monitor,
  groups,
  channels = [],
  channelsUnavailable = false,
  errors,
  onChange,
}: {
  draft: MonitorDraft;
  monitor: Monitor | null;
  groups: readonly MonitorGroup[];
  channels?: readonly NotificationChannel[];
  channelsUnavailable?: boolean;
  errors: MonitorErrors;
  onChange: (patch: Partial<MonitorDraft>) => void;
}) {
  const kept = keepsCredentials(monitor, d.authType);
  const setHeader = (i: number, patch: Partial<HeaderDraft>) => {
    onChange({ headers: d.headers.map((h, j) => (j === i ? { ...h, ...patch } : h)) });
  };
  return (
    <div className="flex flex-col gap-6">
      <Section title="General">
        <Field id="monitor-name" label="Friendly name" error={errors.name}>
          <input
            autoFocus
            required
            maxLength={RULES.maxName}
            value={d.name}
            onChange={(e) => { onChange({ name: e.target.value }); }}
            {...describe("name", errors)}
            className={INPUT}
          />
        </Field>
        <Field
          id="monitor-url"
          label="URL"
          error={errors.url}
          help={
            hasQueryString(d.url)
              ? "Warning: the query string is stored as written and shown to anyone who manages notifications. It is not sealed like a secret: put tokens in a header or in Authentication instead."
              : "http:// or https://, with no user name or password."
          }
        >
          <input
            type="url"
            required
            autoComplete="off"
            spellCheck={false}
            placeholder="https://"
            value={d.url}
            onChange={(e) => { onChange({ url: e.target.value }); }}
            {...describe("url", errors)}
            className={INPUT}
          />
        </Field>
        <Field id="monitor-group_id" label="Group" error={errors.group_id}>
          <select
            value={d.groupID}
            onChange={(e) => { onChange({ groupID: e.target.value }); }}
            {...describe("group_id", errors)}
            className={INPUT}
          >
            <option value="">No group</option>
            {groups.map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
          </select>
        </Field>
        <Field id="monitor-description" label="Description" error={errors.description}>
          <textarea
            rows={2}
            maxLength={RULES.maxDescription}
            value={d.description}
            onChange={(e) => { onChange({ description: e.target.value }); }}
            {...describe("description", errors)}
            className={INPUT}
          />
        </Field>
        <Field id="monitor-tags" label="Tags" error={errors.tags} help="Separated by commas.">
          <input
            value={d.tags}
            placeholder="prod, shop"
            onChange={(e) => { onChange({ tags: e.target.value }); }}
            {...describe("tags", errors)}
            className={INPUT}
          />
        </Field>
      </Section>

      <Section title="Request">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="monitor-method" label="Method" error={errors.method}>
            <select
              value={d.method}
              onChange={(e) => { onChange({ method: e.target.value as MonitorMethod }); }}
              {...describe("method", errors)}
              className={INPUT}
            >
              <option value="GET">GET</option>
              <option value="HEAD">HEAD</option>
              <option value="POST">POST</option>
            </select>
          </Field>
          <NumberField field="interval_seconds" label="Interval (seconds)" value={d.interval} errors={errors}
            min={RULES.minInterval} max={RULES.maxInterval} help={`At least ${RULES.minInterval}.`}
            onChange={(interval) => { onChange({ interval }); }} />
          <NumberField field="retries" label="Retries" value={d.retries} errors={errors} min={0} max={RULES.maxRetries}
            help="Failed checks in a row before the monitor is down."
            onChange={(retries) => { onChange({ retries }); }} />
          <NumberField field="retry_interval_seconds" label="Retry interval (seconds)" value={d.retryInterval} errors={errors}
            min={RULES.minInterval} max={RULES.maxInterval} onChange={(retryInterval) => { onChange({ retryInterval }); }} />
          <NumberField field="timeout_seconds" label="Timeout (seconds)" value={d.timeout} errors={errors} min={1}
            max={RULES.maxTimeout} help="Shorter than the interval." onChange={(timeout) => { onChange({ timeout }); }} />
          <NumberField field="max_redirects" label="Maximum redirects" value={d.maxRedirects} errors={errors} min={0}
            max={RULES.maxRedirects} onChange={(maxRedirects) => { onChange({ maxRedirects }); }} />
        </div>
        <Field id="monitor-accepted_status" label="Accepted status codes" error={errors.accepted_status}
          help='Codes or ranges, separated by commas, such as "200-299, 301".'>
          <input
            value={d.acceptedStatus}
            onChange={(e) => { onChange({ acceptedStatus: e.target.value }); }}
            {...describe("accepted_status", errors)}
            className={INPUT}
          />
        </Field>
        <Field id="monitor-keyword" label="Keyword (optional)" error={errors.keyword}
          help="The check fails unless the first 1 MiB of the response contains this text.">
          <input
            value={d.keyword}
            maxLength={RULES.maxKeyword}
            onChange={(e) => { onChange({ keyword: e.target.value }); }}
            {...describe("keyword", errors)}
            className={INPUT}
          />
        </Field>
      </Section>

      <Section title="Authentication and headers">
        <p className="-mt-2 text-xs text-text-muted">
          Credentials, header values and the body are sealed on the server and never shown again.
          {monitor?.secret_configured ? " A secret is configured for this monitor." : null}
        </p>
        <Field id="monitor-auth_type" label="Authentication" error={errors.auth_type}>
          <select
            value={d.authType}
            onChange={(e) => { onChange({ authType: e.target.value as MonitorAuthType }); }}
            {...describe("auth_type", errors)}
            className={INPUT}
          >
            <option value="none">None</option>
            <option value="basic">Basic</option>
            <option value="bearer">Bearer token</option>
          </select>
        </Field>
        {d.authType === "basic" ? (
          <div className="grid gap-4 sm:grid-cols-2">
            <SecretInput field="basic_username" label="User name" type="text" value={d.basicUsername} kept={kept}
              errors={errors} onChange={(basicUsername) => { onChange({ basicUsername }); }} />
            <SecretInput field="basic_password" label="Password" value={d.basicPassword} kept={kept} errors={errors}
              onChange={(basicPassword) => { onChange({ basicPassword }); }} />
          </div>
        ) : null}
        {d.authType === "bearer" ? (
          <SecretInput field="bearer_token" label="Token" value={d.bearerToken} kept={kept} errors={errors}
            onChange={(bearerToken) => { onChange({ bearerToken }); }} />
        ) : null}

        <div className="flex flex-col gap-2">
          <span className="text-sm text-text" id="monitor-headers-label">Custom headers</span>
          {d.headers.length === 0 ? <p className="text-xs text-text-muted">No custom headers.</p> : null}
          {d.headers.map((h, i) => (
            <div key={i} className="flex flex-wrap items-center gap-2" role="group" aria-label={`Header ${i + 1}`}>
              <input
                aria-label={`Header ${i + 1} name`}
                value={h.name}
                readOnly={h.stored}
                spellCheck={false}
                placeholder="X-Api-Key"
                onChange={(e) => { setHeader(i, { name: e.target.value }); }}
                className={`${INPUT} min-w-0 flex-1`}
              />
              <input
                aria-label={`Header ${i + 1} value`}
                type="password"
                autoComplete="off"
                spellCheck={false}
                value={h.value}
                placeholder={h.stored ? "Configured — leave empty to keep" : "Value"}
                onChange={(e) => { setHeader(i, { value: e.target.value }); }}
                className={`${INPUT} min-w-0 flex-1`}
              />
              <button
                type="button"
                aria-label={`Remove header ${h.name || i + 1}`}
                onClick={() => { onChange({ headers: d.headers.filter((_, j) => j !== i) }); }}
                className="rounded-md p-2 text-text-muted hover:bg-surface-hover hover:text-text"
              >
                <X size={16} aria-hidden="true" />
              </button>
            </div>
          ))}
          <div>
            <Button
              type="button"
              size="sm"
              icon={Plus}
              disabled={d.headers.length >= RULES.maxHeaders}
              onClick={() => { onChange({ headers: [...d.headers, { name: "", value: "", stored: false }] }); }}
            >
              Add header
            </Button>
          </div>
          <p id="monitor-headers-error" aria-live="polite" className="text-xs text-danger">{errors.headers}</p>
        </div>
      </Section>

      <Section title="Body">
        <Field id="monitor-body" label="Request body (POST only)" error={errors.body}
          help={monitor
            ? "Leave empty to keep any stored body. Changing the method away from POST removes it."
            : undefined}>
          <textarea
            rows={4}
            spellCheck={false}
            autoComplete="off"
            disabled={d.method !== "POST"}
            value={d.body}
            onChange={(e) => { onChange({ body: e.target.value }); }}
            {...describe("body", errors)}
            className={`${INPUT} font-mono`}
          />
        </Field>
        <Field id="monitor-body_encoding" label="Body encoding" error={errors.body_encoding}>
          <select
            value={d.bodyEncoding}
            disabled={d.method !== "POST"}
            onChange={(e) => { onChange({ bodyEncoding: e.target.value as MonitorBodyEncoding }); }}
            {...describe("body_encoding", errors)}
            className={INPUT}
          >
            <option value="json">JSON</option>
            <option value="form">Form</option>
            <option value="text">Text</option>
          </select>
        </Field>
      </Section>

      <Section title="Advanced">
        <label className="flex items-center gap-2 text-sm text-text">
          <input type="checkbox" checked={d.certNotify} onChange={(e) => { onChange({ certNotify: e.target.checked }); }} />
          Notify before the TLS certificate expires
        </label>
        <div className="grid gap-4 sm:grid-cols-2">
          <NumberField field="cert_warn_days" label="Warning (days before)" value={d.certWarnDays} errors={{}} min={1}
            max={365} onChange={(certWarnDays) => { onChange({ certWarnDays }); }} />
          <NumberField field="cert_crit_days" label="Critical (days before)" value={d.certCritDays} errors={errors} min={0}
            max={364} onChange={(certCritDays) => { onChange({ certCritDays }); }} />
        </div>
      </Section>

      <Section title="Notifications">
        <ChannelPicker
          channels={channels}
          unavailable={channelsUnavailable}
          selected={selectedChannels(d, channels)}
          onChange={(channelIDs) => { onChange({ channelIDs }); }}
        />
      </Section>
    </div>
  );
}

/** Which channels the monitor alerts. None selected means it alerts no
 *  one, and says so. */
export function ChannelPicker({
  channels,
  unavailable,
  selected,
  onChange,
}: {
  channels: readonly NotificationChannel[];
  unavailable: boolean;
  selected: readonly string[];
  onChange: (channelIDs: string[]) => void;
}) {
  if (unavailable) return <p className="text-sm text-text-muted">Notification channels cannot be loaded right now.</p>;
  const toggle = (id: string, on: boolean) => {
    onChange(on ? [...selected.filter((s) => s !== id), id] : selected.filter((s) => s !== id));
  };
  return (
    <div role="group" aria-label="Notification channels" aria-describedby="monitor-channels-help" className="flex flex-col gap-2">
      {channels.length === 0 ? (
        <p className="text-sm text-text-muted">No notification channels exist yet. Add one on the Notifications page.</p>
      ) : (
        channels.map((c) => (
          <label key={c.id} className="flex items-center gap-2 text-sm text-text">
            <input type="checkbox" checked={selected.includes(c.id)} onChange={(e) => { toggle(c.id, e.target.checked); }} />
            <span>{c.name}</span>
            <span className="text-xs text-text-muted">{c.type}{c.enabled ? "" : ", disabled"}</span>
          </label>
        ))
      )}
      {selected.length === 0 ? (
        <p id="monitor-channels-help" role="status" className="text-sm text-warning">
          No notifications configured: this website alerts no one when it goes down or its certificate expires.
        </p>
      ) : (
        <p id="monitor-channels-help" className="text-xs text-text-muted">
          Alerts for this website go to the selected channels only. Channels marked as a monitor default start selected on new websites.
        </p>
      )}
    </div>
  );
}

export function DeleteMonitorConfirm({
  name, pending, error, onClose, onConfirm,
}: {
  name: string;
  pending: boolean;
  error: string | null;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Modal title="Delete website" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Delete <span className="font-medium text-text">{name}</span>? Its checks, uptime history and events are deleted with it.
      </p>
      <p aria-live="polite" className="mt-3 text-sm text-danger">{error}</p>
      <ModalActions>
        <Button type="button" variant="secondary" autoFocus onClick={onClose}>Cancel</Button>
        <Button type="button" variant="danger" icon={Trash2} disabled={pending} onClick={onConfirm}>
          {pending ? "Deleting…" : "Delete"}
        </Button>
      </ModalActions>
    </Modal>
  );
}
