import { AlertTriangle } from "lucide-react";
import type { ImageSummary } from "./containerModel";
import { Card, CardHeader } from "../../components/Card";

/**
 * The images this host is actually running.
 *
 * Built from the containers rather than from the daemon's image list, which is
 * a deliberate scope limit: Atlas collects `docker.images.count` and
 * `docker.images.size` as aggregates only, so per-image size, age and orphan
 * status are not available. Rather than render four panels of "unavailable",
 * this shows what the container inventory genuinely supports — which images
 * are in use, how widely they are shared, and which tags disagree with
 * themselves.
 *
 * That last one is the panel worth having. Two containers reporting the same
 * tag while running different digests is the confusion an incident needs to
 * resolve quickly, and it is invisible in `docker ps`.
 */
export function ImageDistribution({
  images,
  selected,
  onSelect,
}: {
  images: ImageSummary[];
  selected: string | null;
  onSelect: (reference: string | null) => void;
}) {
  const shared = images.filter((i) => i.total > 1).length;
  const drifted = images.filter((i) => i.digests.length > 1);

  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="Images in use"
        action={
          <span className="text-xs text-text-muted">
            {images.length} distinct · {shared} shared by more than one container
          </span>
        }
      />

      {drifted.length > 0 ? (
        <div className="surface-warn mb-3 flex items-start gap-2.5 rounded-lg p-3">
          <AlertTriangle size={14} className="mt-0.5 shrink-0 text-warning" aria-hidden="true" />
          <div className="text-xs leading-relaxed">
            <strong className="text-text">
              {drifted.length} tag{drifted.length === 1 ? "" : "s"} resolve to more than one build
            </strong>
            <p className="mt-0.5 text-text-muted">
              Containers claiming the same image tag are running different digests — one was started
              before the tag moved. {drifted.map((d) => d.reference).join(", ")}
            </p>
          </div>
        </div>
      ) : null}

      <ul className="flex flex-col">
        {images.map((img) => (
          <li key={img.reference}>
            <button
              type="button"
              onClick={() => { onSelect(selected === img.reference ? null : img.reference); }}
              aria-pressed={selected === img.reference}
              className={`flex w-full items-center gap-3 rounded-lg px-2 py-2 text-left transition-colors ${
                selected === img.reference ? "bg-primary/10" : "hover:bg-surface-hover"
              }`}
            >
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm text-text" title={img.reference}>
                  {img.repository}
                  <span className="text-text-subtle">:{img.tag}</span>
                </span>
                {img.digests.length > 1 ? (
                  <span className="text-[11px] text-warning">
                    {img.digests.length} different builds
                  </span>
                ) : null}
              </span>

              {/* Share of the container inventory, scaled against the most-used
                  image so the column is legible whether the leader has two
                  containers or twenty. */}
              <span className="h-1.5 w-24 shrink-0 overflow-hidden rounded-full bg-surface-hover">
                <span
                  className="block h-full rounded-full bg-primary"
                  style={{
                    width: `${String(Math.max((img.total / (images[0]?.total ?? 1)) * 100, 4))}%`,
                  }}
                />
              </span>

              <span className="w-24 shrink-0 text-right text-xs tabular-nums text-text-muted">
                {img.running > 0 ? (
                  <span className="text-success">{img.running} running</span>
                ) : (
                  <span className="text-text-subtle">none running</span>
                )}
                <span className="block text-[11px] text-text-subtle">
                  {img.total} container{img.total === 1 ? "" : "s"}
                </span>
              </span>
            </button>
          </li>
        ))}
      </ul>

      <p className="mt-3 border-t border-border pt-2.5 text-[11px] leading-relaxed text-text-subtle">
        Only images with a container are listed. Image size, age and orphaned images need per-image
        inventory, which Atlas records as a daemon-wide total rather than per image — so they are
        omitted here rather than estimated.
      </p>
    </Card>
  );
}
