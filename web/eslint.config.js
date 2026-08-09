import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

/**
 * ESLint configuration for the Atlas frontend.
 *
 * Type-aware rules are enabled. They cost a slower lint run and repay it by
 * catching the class of mistake that matters most in a monitoring UI: a
 * floating promise that swallows a failed request, or an unsafe `any` that
 * lets a missing API field render as blank rather than fail.
 */
export default tseslint.config(
  { ignores: ["dist", "node_modules"] },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      ...tseslint.configs.strictTypeChecked,
      ...tseslint.configs.stylisticTypeChecked,
    ],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
      parserOptions: {
        project: ["./tsconfig.app.json", "./tsconfig.node.json"],
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],

      // An unhandled rejection in a data-fetching UI shows the operator stale
      // data with no indication that the refresh failed.
      "@typescript-eslint/no-floating-promises": "error",
      "@typescript-eslint/no-misused-promises": "error",

      // Numbers stringify unambiguously, and an operations UI is mostly
      // numbers. Objects and nullish values stay banned, which is where the
      // rule earns its keep.
      "@typescript-eslint/restrict-template-expressions": [
        "error",
        { allowNumber: true },
      ],

      // Unused values are usually the residue of an incomplete change.
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
    },
  },
);
