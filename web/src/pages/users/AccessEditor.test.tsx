import { isValidElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { Grant, UserAccessResponse } from "../../api/types";
import { toDraft } from "../accessEditor";
import { AccessGrid, SaveBar } from "./AccessEditor";
import { RoleGrantsTable } from "./RoleGrants";

/** Every element in a rendered-to-elements tree, depth first. SaveBar has no
 *  hooks, so calling it returns its element tree without a renderer. */
function elements(node: ReactNode): ReactElement<Record<string, unknown>>[] {
  if (Array.isArray(node)) return node.flatMap(elements);
  if (!isValidElement<Record<string, unknown>>(node)) return [];
  return [node, ...elements(node.props.children as ReactNode)];
}

function buttonLabelled(tree: ReactNode, label: string) {
  return elements(tree).find((e) => elements(e.props.children as ReactNode).length === 0 && e.props.children === label);
}

describe("SaveBar", () => {
  const noop = () => undefined;

  it("shows the change count, and nothing with no changes", () => {
    expect(renderToStaticMarkup(<SaveBar count={3} saving={false} blocked={false} onSave={noop} onDiscard={noop} />)).toContain(
      "3 changes",
    );
    expect(renderToStaticMarkup(<SaveBar count={1} saving={false} blocked={false} onSave={noop} onDiscard={noop} />)).toContain(
      "1 change<",
    );
    expect(renderToStaticMarkup(<SaveBar count={0} saving={false} blocked={false} onSave={noop} onDiscard={noop} />)).toBe("");
  });

  it("Discard calls onDiscard and not onSave", () => {
    const onSave = vi.fn();
    const onDiscard = vi.fn();
    const tree = SaveBar({ count: 2, saving: false, blocked: false, onSave, onDiscard });
    const discard = buttonLabelled(tree, "Discard");
    expect(discard).toBeDefined();
    (discard?.props.onClick as () => void)();
    expect(onDiscard).toHaveBeenCalledOnce();
    expect(onSave).not.toHaveBeenCalled();
  });

  it("Save is disabled while the draft has problems", () => {
    const tree = SaveBar({ count: 2, saving: false, blocked: true, onSave: noop, onDiscard: noop });
    expect(buttonLabelled(tree, "Save")?.props.disabled).toBe(true);
    expect(buttonLabelled(tree, "Discard")?.props.disabled).toBe(false);
  });
});

describe("RoleGrantsTable", () => {
  const at = "2026-01-01T00:00:00Z";
  const grants: Grant[] = [
    { id: "g-super", role: "superadmin", fleet_wide: true, granted_at: at },
    { id: "g-admin", role: "admin", fleet_wide: true, granted_at: at },
  ];

  it("never renders a Revoke control on a superadmin grant", () => {
    const html = renderToStaticMarkup(
      <RoleGrantsTable grants={grants} scopeLabel={() => "fleet-wide"} guardFor={() => ({ disabled: false })} onRevoke={() => undefined} />,
    );
    expect(html.match(/>Revoke</g)).toHaveLength(1);
    const superRow = html.slice(html.indexOf(">superadmin<"), html.indexOf(">admin<"));
    expect(superRow).not.toContain("Revoke");
  });
});

describe("AccessGrid", () => {
  const server: UserAccessResponse = {
    version: "v1",
    roles: [{ id: "r1", role: "viewer", node_id: "n1", fleet_wide: false, granted_at: "2026-01-01T00:00:00Z" }],
    pages: [{ id: "p1", page: "ports", fleet_wide: true, granted_at: "2026-01-01T00:00:00Z" }],
    bundles: [
      { id: "b1", role_access: "containers", fleet_wide: true, pages: ["containers"], granted_at: "2026-01-01T00:00:00Z" },
    ],
  };
  const html = renderToStaticMarkup(
    <AccessGrid server={server} draft={toDraft(server)} nodes={[{ node_id: "n1", hostname: "web-1" }, { node_id: "n2", hostname: "db-1" }]} readOnly={false} onEdit={() => undefined} />,
  );

  it("never offers superadmin as a role", () => {
    expect(html).not.toContain("superadmin");
  });

  it("locks bundle pages and pages the fleet-wide row covers", () => {
    expect(html).toContain("(from bundle containers)");
    expect(html).toContain("(via fleet-wide)");
    expect(html).toContain("needs a role with user management");
  });
});

describe("AccessGrid for a non-superadmin", () => {
  const server: UserAccessResponse = {
    version: "v1",
    roles: [{ id: "r1", role: "admin", fleet_wide: true, granted_at: "2026-01-01T00:00:00Z" }],
    pages: [],
    bundles: [],
  };
  const render = (canChangeAdmin: boolean) =>
    renderToStaticMarkup(
      <AccessGrid server={server} draft={toDraft(server)} nodes={[{ node_id: "n1", hostname: "web-1" }]} readOnly={false} canChangeAdmin={canChangeAdmin} onEdit={() => undefined} />,
    );

  it("disables the admin option and the admin row, with the hint", () => {
    const html = render(false);
    expect(html.match(/<option value="admin" disabled=""/g)).toHaveLength(2);
    expect(html).toMatch(/aria-label="Role for fleet-wide"[^>]*disabled=""/);
    expect(html).not.toMatch(/aria-label="Role for n1"[^>]*disabled=""/);
    expect(html).toContain("Only a superadmin can change admin access");
  });

  it("a superadmin sees no change", () => {
    const html = render(true);
    expect(html).not.toContain('<option value="admin" disabled');
    expect(html).not.toContain("Only a superadmin can change admin access");
  });
});
