#!/usr/bin/env node
/**
 * Optimises the design-asset library into web/public/assets.
 *
 * The source pack is ~95MB of full-resolution PNGs. Atlas is read during
 * incidents, frequently over a slow link into a bastion host — the same
 * reasoning that put a chunk budget in vite.config.ts — so shipping those
 * unchanged would be indefensible. Every asset here is resized to the largest
 * size the UI actually renders it at (times two, for retina) and re-encoded
 * as WebP.
 *
 * This is a build step rather than a one-off: the output is derived, so it
 * stays reproducible when an asset is replaced, and `npm run assets` is the
 * only thing anyone has to remember.
 *
 * Usage: npm run assets
 */
import { mkdir, readdir, rm } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import sharp from "sharp";

const here = dirname(fileURLToPath(import.meta.url));
const SRC = join(here, "../../Atlas_Design_Assets");
const OUT = join(here, "../public/assets");

/**
 * Each entry is [source, destination, width, options].
 *
 * Widths are chosen from how the asset is actually used, not from what the
 * source happens to be: an illustration rendered in a 260px empty state does
 * not need 1536px, and a logo mark drawn at 24px certainly does not need
 * 1254px.
 */
/**
 * How much of an illustration's height is artwork rather than baked-in UI
 * text. Measured from the source pack; every illustration in it shares the
 * same layout.
 */
const ILLUSTRATION_KEEP = 0.62;

const JOBS = [
  // Brand. The mark is drawn at 24-32px in the sidebar; 128 covers retina
  // with room to spare.
  ["logo/Primary/atlas-mark.png", "logo/mark.webp", 192, { keyWhite: true }],
  ["logo/Primary/atlas-logo-dark.png", "logo/wordmark-dark.webp", 320],
  ["logo/Primary/atlas-logo-light.png", "logo/wordmark-light.webp", 320],
  ["logo/Primary/favicon.png", "logo/favicon.png", 64, { keepPng: true }],
  ["logo/Primary/apple-touch-icon.png", "logo/apple-touch-icon.png", 180, { keepPng: true }],

  // Empty states, rendered around 240-280px.
  ...[
    "no-data",
    "no-servers",
    "no-containers",
    "no-services",
    "no-logs",
    "no-alerts",
    "no-reports",
    "no-search-results",
  ].map((n) => [`illustrations/empty/${n}.png`, `empty/${n}.webp`, 560, { illustration: true }]),

  // Error and health screens, rendered larger — they are the whole page.
  ...["404", "403", "401", "500", "server-offline"].map((n) => [
    `illustrations/errors/${n}.png`,
    `errors/${n}.webp`,
    720,
    { illustration: true },
  ]),
  ...["healthy", "warning", "critical", "maintenance"].map((n) => [
    `illustrations/health/${n}.png`,
    `health/${n}.webp`,
    560,
    { illustration: true },
  ]),
  ...["infrastructure", "monitor", "analytics", "automation"].map((n) => [
    `illustrations/onboarding/${n}.png`,
    `onboarding/${n}.webp`,
    640,
    { illustration: true },
  ]),

  // Textures. Overlays that sit above a background and below content, so
  // they are compressed hard — nobody reads a noise field.
  // Noise and grain tile, so they are cropped to a square rather than scaled:
  // downsizing the whole 1536px frame would blur the grain into mush, which
  // is the one thing a noise overlay must not do.
  ["textures/noise.png", "textures/noise.webp", 256, { crop: 256, quality: 60 }],
  ["textures/grain.png", "textures/grain.webp", 256, { crop: 256, quality: 60 }],
  // Frost and vignette are framed effects, not tiles, so they scale whole.
  ["textures/glass-frost.png", "textures/glass-frost.webp", 900, { quality: 62 }],
  ["textures/vignette.png", "textures/vignette.webp", 1200, { quality: 60 }],

  // Patterns. One is layered under every page at very low opacity — the
  // single most identifiable thing about the language. Assigned per page in
  // lib/pageChrome.ts, following the library's stated intent: grid for
  // dashboards, hex for infrastructure, circuit for networking, and so on.
  ["patterns/grid.png", "patterns/grid.webp", 512, { quality: 70 }],
  ["patterns/dots.png", "patterns/dots.webp", 512, { quality: 70 }],
  ["patterns/hex.png", "patterns/hex.webp", 512, { quality: 70 }],
  ["patterns/circuit.png", "patterns/circuit.webp", 640, { quality: 70 }],
  ["patterns/blueprint.png", "patterns/blueprint.webp", 640, { quality: 70 }],
  ["backgrounds/dashboard/topography.png", "patterns/topography.webp", 900, { quality: 68 }],

  // Hero backgrounds. Each page's hero gets one; a plain black banner is
  // explicitly not an option where one of these exists.
  ["backgrounds/hero/aurora-purple.png", "heroes/aurora.webp", 1400, { quality: 66 }],
  ["backgrounds/hero/digital-horizon.png", "heroes/horizon.webp", 1400, { quality: 66 }],
  ["backgrounds/hero/digital-ocean.png", "heroes/ocean.webp", 1400, { quality: 66 }],
  ["backgrounds/hero/wireframe-terrain.png", "heroes/terrain.webp", 1400, { quality: 66 }],
  ["backgrounds/dashboard/mesh.png", "heroes/mesh.webp", 1400, { quality: 66 }],
  ["backgrounds/dashboard/polygon.png", "heroes/polygon.webp", 1400, { quality: 66 }],
  ["backgrounds/login/login-nebula.png", "heroes/nebula.webp", 1400, { quality: 66 }],
  ["backgrounds/login/login-glass.png", "heroes/glass.webp", 1400, { quality: 66 }],

  // Gradient washes, used as overlay layers rather than as flat fills.
  ["gradients/aurora-purple.png", "gradients/aurora-purple.webp", 900, { quality: 62 }],
  ["gradients/aurora-blue.png", "gradients/aurora-blue.webp", 900, { quality: 62 }],
  ["gradients/mesh-violet.png", "gradients/mesh-violet.webp", 900, { quality: 62 }],
  ["gradients/mesh-cyan.png", "gradients/mesh-cyan.webp", 900, { quality: 62 }],
  ["gradients/midnight.png", "gradients/midnight.webp", 900, { quality: 62 }],
  ["gradients/radial-glow.png", "gradients/radial-glow.webp", 900, { quality: 62 }],

  // Maps. Reserved for infrastructure-wide views — the dashboard, nodes and
  // status heroes — never dropped into an arbitrary card.
  ["backgrounds/maps/world-dots.png", "maps/world-dots.webp", 1400, { quality: 70 }],
  ["backgrounds/maps/world-glow.png", "maps/world-glow.webp", 1400, { quality: 68 }],
  ["backgrounds/maps/world-grid.png", "maps/world-grid.webp", 1400, { quality: 68 }],
  ["backgrounds/maps/world-network.png", "maps/world-network.webp", 1400, { quality: 68 }],
  ["backgrounds/maps/world-night-lights.png", "maps/world-night.webp", 1400, { quality: 68 }],
];

async function main() {
  await rm(OUT, { recursive: true, force: true });

  let total = 0;
  for (const [src, dest, width, opts = {}] of JOBS) {
    const from = join(SRC, src);
    const to = join(OUT, dest);
    await mkdir(dirname(to), { recursive: true });

    let img = sharp(from);

    if (opts.keyWhite) {
      // The logo files ship on an opaque near-white ground, which is fine on
      // a light page and a white slab on a dark one. Keying that ground out
      // is what lets the official mark be used unmodified on any surface —
      // the alternative was redrawing it, which the asset rules rule out.
      //
      // Only low-saturation near-white pixels are cleared, so the mark's own
      // blues are untouched. The counter inside the "A" is white and does go
      // transparent, which is correct: it is a hole in the glyph.
      const { data, info } = await sharp(from)
        .ensureAlpha()
        .raw()
        .toBuffer({ resolveWithObject: true });

      for (let i = 0; i < data.length; i += info.channels) {
        const r = data[i];
        const g = data[i + 1];
        const b = data[i + 2];
        const max = Math.max(r, g, b);
        const min = Math.min(r, g, b);
        if (min > 224 && max - min < 18) data[i + 3] = 0;
      }

      img = sharp(data, { raw: { width: info.width, height: info.height, channels: info.channels } })
        .trim({ threshold: 1 });
    }

    if (opts.crop) {
      img = img.extract({ left: 0, top: 0, width: opts.crop, height: opts.crop });
    } else {
      if (opts.illustration) {
        // The source "illustrations" are whole-screen mockups: artwork on
        // top, then a baked-in heading, body copy, and buttons. Rendering
        // that inside Atlas would duplicate every heading, show captions
        // written for a different product, and — worse — display buttons
        // for actions a read-only tool does not have. Only the artwork is
        // usable, so the text half is cropped away and the surrounding
        // whitespace trimmed off what remains.
        const meta = await sharp(from).metadata();
        const height = Math.round((meta.height ?? 0) * ILLUSTRATION_KEEP);
        // Two passes: sharp cannot trim within the same pipeline that
        // extracted, because trim measures the *input* image's borders.
        const cropped = await sharp(from)
          .extract({ left: 0, top: 0, width: meta.width ?? 0, height })
          .toBuffer();
        img = sharp(cropped).trim({ threshold: 12 });
      }
      img = img.resize({ width, withoutEnlargement: true });
    }

    img = opts.keepPng
      ? img.png({ compressionLevel: 9, palette: true })
      : img.webp({ quality: opts.quality ?? 82 });

    const { size } = await img.toFile(to);
    total += size;
    console.log(`  ${dest.padEnd(34)} ${(size / 1024).toFixed(0).padStart(5)} KB`);
  }

  console.log(`\n  ${JOBS.length} assets · ${(total / 1024 / 1024).toFixed(2)} MB total`);
}

async function assertSourceExists() {
  try {
    await readdir(SRC);
  } catch {
    console.error(`Design assets not found at ${SRC}`);
    console.error("This step is optional; the UI degrades to no imagery without it.");
    process.exit(1);
  }
}

await assertSourceExists();
await main();
