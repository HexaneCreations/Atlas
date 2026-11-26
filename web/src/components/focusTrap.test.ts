import { describe, expect, it } from "vitest";
import { focusAfterTab } from "./focusTrap";

describe("focusAfterTab", () => {
  const inside = ["first", "middle", "last"];

  it("wraps at both ends and otherwise leaves Tab alone", () => {
    expect(focusAfterTab(inside, "last", false)).toBe("first");
    expect(focusAfterTab(inside, "first", true)).toBe("last");
    expect(focusAfterTab(inside, "middle", false)).toBeNull();
    expect(focusAfterTab(inside, "middle", true)).toBeNull();
  });

  it("brings focus back in from outside", () => {
    expect(focusAfterTab(inside, "elsewhere", false)).toBe("first");
    expect(focusAfterTab(inside, "elsewhere", true)).toBe("last");
    expect(focusAfterTab([], "elsewhere", false)).toBeNull();
  });
});
