import { Link } from "react-router";
import { AlertTriangle, ArrowRight, Check, EyeOff } from "lucide-react";
import { motion } from "framer-motion";
import type { Coverage, Finding, Severity } from "./findings";
import { healthArt, patterns } from "../../lib/assets";

/**
 * Attention Required: the answer to "what needs me right now".
 *
 * It is the first thing on the landing page and it is the only panel in Atlas
 * that leads with problems rather than with data. Everything else on Overview
 * describes the machine; this one prosecutes it.
 *
 * When it is empty, that emptiness *is* the product working — so the all-clear
 * is designed as carefully as the alarm, and it states what was checked. An
 * all-clear that cannot tell "nothing is wrong" from "I could not look" is the
 * single most dangerous screen a monitoring tool can render, and on a host
 * without Docker or systemd that is not hypothetical: several sources are
 * genuinely unreadable and the page must say so.
 */

const SEVERITY: Record<
  Severity,
  { label: string; chip: string; rail: string; icon: string }
> = {
  critical: {
    label: "Critical",
    chip: "bg-danger/12 text-danger",
    rail: "bg-danger",
    icon: "text-danger",
  },
  warning: {
    label: "Warning",
    chip: "bg-warning/12 text-warning",
    rail: "bg-warning",
    icon: "text-warning",
  },
};

export function AttentionRequired({
  findings,
  coverage,
}: {
  findings: Finding[];
  coverage: Coverage[];
}) {
  const critical = findings.filter((f) => f.severity === "critical").length;
  const blind = coverage.filter((c) => !c.ok);

  if (findings.length === 0) {
    return <AllClear coverage={coverage} />;
  }

  return (
    <section
      className="elev-3 relative isolate mb-6 overflow-hidden rounded-2xl"
      aria-labelledby="attention-heading"
    >
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 -z-10 opacity-[0.05]"
        style={{ backgroundImage: `url(${patterns.grid})`, backgroundSize: "300px" }}
      />
      {/* A wash keyed to the worst severity present, so the panel's
          temperature is readable before a single word is. */}
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-40"
        style={{
          background: `linear-gradient(to bottom, ${
            critical > 0 ? "var(--danger)" : "var(--warning)"
          }, transparent)`,
          opacity: 0.09,
        }}
      />

      <header className="flex flex-wrap items-center gap-3 border-b border-border px-5 py-4">
        <AlertTriangle
          size={18}
          className={critical > 0 ? "text-danger" : "text-warning"}
          aria-hidden="true"
        />
        <h2 id="attention-heading" className="text-card-title font-semibold text-text">
          Attention required
        </h2>
        <span className="text-sm text-text-muted">
          {findings.length === 1 ? "1 finding" : `${String(findings.length)} findings`}
          {critical > 0 ? ` · ${String(critical)} critical` : ""}
        </span>
        {blind.length > 0 ? (
          <span className="ml-auto flex items-center gap-1.5 text-xs text-text-subtle">
            <EyeOff size={12} aria-hidden="true" />
            {blind.length} source{blind.length === 1 ? "" : "s"} not readable
          </span>
        ) : null}
      </header>

      <ul className="divide-y divide-border">
        {findings.map((f, i) => (
          <FindingRow key={f.id} finding={f} index={i} />
        ))}
      </ul>
    </section>
  );
}

function FindingRow({ finding: f, index }: { finding: Finding; index: number }) {
  const s = SEVERITY[f.severity];

  return (
    <motion.li
      initial={{ opacity: 0, y: 6 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ delay: Math.min(index * 0.04, 0.24), duration: 0.28 }}
      className="relative"
    >
      {/* Severity rail: the colour runs the full height of the row, so a long
          list is scannable by edge alone without reading any titles. */}
      <span aria-hidden="true" className={`absolute inset-y-0 left-0 w-[3px] ${s.rail}`} />

      <Link
        to={f.to}
        className="group flex gap-4 px-5 py-4 transition-colors hover:bg-surface-hover"
      >
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className={`rounded px-1.5 py-0.5 text-[10px] font-semibold tracking-wide uppercase ${s.chip}`}>
              {s.label}
            </span>
            <h3 className="text-sm font-semibold text-text">{f.title}</h3>
            <span className="text-xs text-text-subtle">{f.source}</span>
          </div>

          <p className="mt-1.5 max-w-3xl text-sm leading-relaxed text-text-muted">{f.detail}</p>

          {f.evidence.length > 0 ? (
            // The named instances are what make a finding checkable rather
            // than a claim; without them "3 units failed" is unactionable.
            <ul className="mt-2.5 flex flex-wrap gap-1.5">
              {f.evidence.map((e) => (
                <li
                  key={e}
                  className="elev-1 truncate rounded px-2 py-1 font-mono text-[11px] text-text-muted"
                  title={e}
                >
                  {e}
                </li>
              ))}
            </ul>
          ) : null}
        </div>

        <ArrowRight
          size={16}
          aria-hidden="true"
          className="mt-1 shrink-0 self-start text-text-subtle transition-transform group-hover:translate-x-0.5 group-hover:text-text-muted"
        />
      </Link>
    </motion.li>
  );
}

/**
 * The all-clear.
 *
 * Deliberately not a green banner and nothing else. It names every source it
 * checked, and names separately every source it could not read — because on
 * this host those lists are both non-empty, and a reader is entitled to know
 * that "no container problems" means "Docker is not installed here".
 */
function AllClear({ coverage }: { coverage: Coverage[] }) {
  const checked = coverage.filter((c) => c.ok);
  const blind = coverage.filter((c) => !c.ok);

  return (
    <section
      className="elev-3 relative isolate mb-6 overflow-hidden rounded-2xl px-6 py-8"
      aria-labelledby="allclear-heading"
    >
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 -z-10 opacity-[0.05]"
        style={{ backgroundImage: `url(${patterns.grid})`, backgroundSize: "300px" }}
      />
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-40"
        style={{ background: "linear-gradient(to bottom, var(--success), transparent)", opacity: 0.08 }}
      />

      <div className="flex flex-col items-center gap-6 sm:flex-row sm:items-start">
        <img
          src={healthArt.healthy}
          alt=""
          aria-hidden="true"
          loading="lazy"
          className="illus w-40 shrink-0"
        />

        <div className="min-w-0 flex-1 text-center sm:text-left">
          <div className="flex items-center justify-center gap-2 sm:justify-start">
            <Check size={18} className="text-success" aria-hidden="true" />
            <h2 id="allclear-heading" className="text-card-title font-semibold text-text">
              Nothing needs attention
            </h2>
          </div>
          <p className="mt-1.5 text-sm leading-relaxed text-text-muted">
            Every source Atlas can read on this host reports normal. This panel fills itself in the
            moment that stops being true.
          </p>

          <dl className="mt-5 flex flex-col gap-3 text-xs sm:flex-row sm:gap-8">
            <div>
              <dt className="eyebrow mb-1.5">Checked</dt>
              <dd className="flex flex-wrap justify-center gap-1.5 sm:justify-start">
                {checked.map((c) => (
                  <span
                    key={c.source}
                    className="flex items-center gap-1 rounded bg-success/10 px-1.5 py-0.5 text-success"
                  >
                    <Check size={10} aria-hidden="true" />
                    {c.source}
                  </span>
                ))}
              </dd>
            </div>

            {blind.length > 0 ? (
              <div>
                <dt className="eyebrow mb-1.5">Not readable</dt>
                <dd className="flex flex-wrap justify-center gap-1.5 sm:justify-start">
                  {blind.map((c) => (
                    <span
                      key={c.source}
                      title={c.reason}
                      className="flex items-center gap-1 rounded bg-surface-hover px-1.5 py-0.5 text-text-subtle"
                    >
                      <EyeOff size={10} aria-hidden="true" />
                      {c.source}
                    </span>
                  ))}
                </dd>
              </div>
            ) : null}
          </dl>

          {blind.length > 0 ? (
            <p className="mt-4 border-t border-border pt-3 text-xs leading-relaxed text-text-subtle">
              “Nothing needs attention” covers the checked sources only. Atlas could not read{" "}
              {blind.map((c) => c.source.toLowerCase()).join(", ")} on this host, and reports that
              rather than counting them as healthy.
            </p>
          ) : null}
        </div>
      </div>
    </section>
  );
}
