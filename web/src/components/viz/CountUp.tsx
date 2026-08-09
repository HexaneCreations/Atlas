import { useEffect, useRef, useState } from "react";

/**
 * A figure that counts up when it first appears.
 *
 * The animation runs on mount only, never on update. That restriction is the
 * whole design: these are live metrics that re-poll every few seconds, and a
 * number that re-animates on every poll is unreadable — the operator watches
 * digits tumble instead of reading a value. Counting up once, as the page
 * arrives, is a flourish; counting up forever is an obstruction.
 *
 * Honours `prefers-reduced-motion` by rendering the final value immediately.
 */
export function CountUp({
  value,
  format,
  durationMs = 650,
}: {
  value: number;
  format: (v: number) => string;
  durationMs?: number;
}) {
  const [display, setDisplay] = useState(value);
  // The value at mount. Later changes bypass the animation entirely.
  const from = useRef(value);
  const animated = useRef(false);

  useEffect(() => {
    if (animated.current) {
      setDisplay(value);
      return;
    }
    animated.current = true;

    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      setDisplay(value);
      return;
    }

    const target = from.current;
    const start = performance.now();
    let frame = 0;

    const tick = (now: number) => {
      const t = Math.min((now - start) / durationMs, 1);
      // Ease out cubic: fast at first, settling gently. A linear count reads
      // as a loading bar rather than as a value arriving.
      setDisplay(target * (1 - Math.pow(1 - t, 3)));
      if (t < 1) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);

    return () => { cancelAnimationFrame(frame); };
  }, [value, durationMs]);

  return <>{format(display)}</>;
}
