import { RotateCcw } from "lucide-react";
import { useNodeAlertRules, useSetNodeAlertRule } from "../../api/queries";
import type { NodeAlertRule } from "../../api/types";
import { useAuth } from "../../auth/AuthContext";
import { Badge } from "../../components/Badge";
import { Button } from "../../components/Button";
import { messageFor } from "../../lib/apiMessage";
import { globalNote, groupByArea, isCustomised } from "../../lib/alertRules";

/** Which alert rules are active on one node. Nothing without
 *  can_manage_notifications; the server checks it on every call. */
export function AlertRules({ nodeID }: { nodeID: string }) {
  const { user } = useAuth();
  const allowed = user?.can_manage_notifications ?? false;
  if (!allowed) return null;
  return <AlertRulesPanel nodeID={nodeID} />;
}

function AlertRulesPanel({ nodeID }: { nodeID: string }) {
  const rules = useNodeAlertRules(nodeID);
  const change = useSetNodeAlertRule(nodeID);
  if (rules.isPending) return <p className="text-xs text-text-muted">Loading alert rules…</p>;
  if (rules.isError) return <p role="alert" className="text-xs text-danger">{messageFor(rules.error)}</p>;
  return (
    <AlertRulesView
      rules={rules.data.rules}
      pendingKey={change.isPending ? change.variables.ruleKey : null}
      error={change.error ? messageFor(change.error) : null}
      onChange={(ruleKey, override) => { change.mutate({ ruleKey, override }); }}
    />
  );
}

export function AlertRulesView({
  rules,
  pendingKey,
  error,
  onChange,
  subject = "node",
}: {
  rules: NodeAlertRule[];
  pendingKey: string | null;
  error: string | null;
  /** override null resets the rule to inherit. */
  onChange: (ruleKey: string, override: boolean | null) => void;
  /** What the rules are shown for, in the notes: "node" or "website". */
  subject?: string;
}) {
  return (
    <section aria-label="Alert rules" className="space-y-4">
      {groupByArea(rules).map((group) => (
        <div key={group.area} role="group" aria-label={group.label}>
          <h3 className="mb-2 text-xs font-semibold tracking-wide text-text-muted uppercase">{group.label}</h3>
          <ul className="space-y-2">
            {group.rules.map((rule) => (
              <AlertRuleRow key={rule.key} rule={rule} subject={subject} busy={pendingKey === rule.key} onChange={onChange} />
            ))}
          </ul>
        </div>
      ))}
      <p aria-live="polite" className="text-xs text-danger">{error}</p>
    </section>
  );
}

function AlertRuleRow({
  rule,
  subject,
  busy,
  onChange,
}: {
  rule: NodeAlertRule;
  subject: string;
  busy: boolean;
  onChange: (ruleKey: string, override: boolean | null) => void;
}) {
  const customised = isCustomised(rule);
  const note = globalNote(rule, subject);
  const descriptionID = `alert-rule-${rule.key}-description`;
  return (
    <li className="rounded-lg border border-border p-3">
      <div className="flex items-start justify-between gap-3">
        <label className="flex min-w-0 items-start gap-2 text-sm text-text">
          <input
            type="checkbox"
            role="switch"
            className="mt-0.5"
            checked={rule.enabled}
            disabled={busy}
            aria-describedby={descriptionID}
            onChange={(e) => { onChange(rule.key, e.target.checked); }}
          />
          <span className="min-w-0">
            <span className="font-medium">{rule.title}</span>
            <span id={descriptionID} className="block text-xs text-text-muted">
              {rule.description}
              {note ? <span className="block">{note}</span> : null}
            </span>
          </span>
        </label>
        <div className="flex shrink-0 items-center gap-1.5">
          <Badge tone={customised ? "info" : "neutral"}>{customised ? "customised" : "inherited"}</Badge>
          <Button
            size="sm"
            icon={RotateCcw}
            disabled={!customised || busy}
            aria-label={`Reset ${rule.title} to the default`}
            onClick={() => { onChange(rule.key, null); }}
          >
            Reset
          </Button>
        </div>
      </div>
    </li>
  );
}
