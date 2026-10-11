import { dirname } from 'path'
import { fileURLToPath } from 'url'
import { FlatCompat } from '@eslint/eslintrc'
import localRules from './eslint-rules/icon-only-button.mjs'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)

const compat = new FlatCompat({
  baseDirectory: __dirname,
})

// no-restricted-syntax is one rule with one options array, so a files block that
// sets it replaces every selector an earlier block set. Each scope therefore
// spreads the full list it wants.

// App source under src/.
const srcSelectors = [
  {
    selector:
      "ExpressionStatement > CallExpression[callee.name='setTimeout'], ExpressionStatement > CallExpression[callee.object.name='window'][callee.property.name='setTimeout']",
    message:
      'Keep the setTimeout handle and clear it in cleanup; a discarded handle leaks on unmount.',
  },
  // no-restricted-properties matches only an identifier object, so it misses window.location.reload.
  {
    selector:
      "MemberExpression[property.name='reload']:matches([object.name='location'], [object.property.name='location'])",
    message: "Use the query's refetch() instead of reloading the page.",
  },
]

// The accelerated clock: src/hooks/use-accelerated-time.ts is the only wall-clock reader.
const wallClockSelectors = [
  {
    selector: "NewExpression[callee.name='Date'][arguments.length=0]",
    message: 'Use the accelerated clock from src/hooks/use-accelerated-time.ts, not new Date().',
  },
  {
    selector: "CallExpression[callee.object.name='Date'][callee.property.name='now']",
    message: 'Use the accelerated clock from src/hooks/use-accelerated-time.ts, not Date.now().',
  },
]

// Playwright specs and helpers under tests/.
const apiConstantSelectors = [
  {
    selector: 'VariableDeclarator[id.name=/^(API_BASE_URL|API_KEY|API_HEADERS|API_URL)$/]',
    message:
      "Import API_BASE_URL, API_KEY and API_HEADERS from 'tests/e2e/helpers/test-api' instead of redeclaring them.",
  },
  {
    selector: "Literal[value='test-api-key-for-ci']",
    message: "The fallback API key lives only in 'tests/e2e/helpers/test-api'. Import API_KEY.",
  },
]

const testSelectors = [
  {
    selector: "Property[key.name='timeout'][value.type='Literal']",
    message:
      'Use a named timeout constant from tests/e2e/helpers/timeouts.ts, or omit it to use the Playwright config default.',
  },
  {
    selector: "Literal[value='networkidle']",
    message:
      "Do not wait for 'networkidle'. Assert on the UI state the test needs (web-first expect).",
  },
  {
    selector: "CallExpression[callee.property.name='waitForTimeout']",
    message:
      'Do not sleep with waitForTimeout. Wait on a condition (web-first expect, waitForResponse).',
  },
  {
    // First literal argument of .locator( containing a CSS class selector:
    // `.text-lg`, `div.border`, or `[class...]`. Attribute selectors such as
    // [data-testid=...] do not match.
    selector:
      "CallExpression[callee.property.name='locator'] > Literal:first-child[value=/(^|[\\s>+~])\\.?[a-zA-Z][\\w-]*\\.[a-zA-Z_-]|^\\.[a-zA-Z_-]|\\[class/]",
    message: 'Locate by role, label, or test id instead of a CSS class.',
  },
]

const srcTestFiles = ['**/__tests__/**', '**/*.test.*']

const eslintConfig = [
  ...compat.extends('next/core-web-vitals', 'next/typescript'),
  { linterOptions: { reportUnusedDisableDirectives: 'error' } },
  {
    files: ['**/*.{jsx,tsx}'],
    plugins: { local: localRules },
    rules: { 'local/icon-only-button': 'error' },
  },
  {
    files: ['src/**/*.{ts,tsx}'],
    ignores: srcTestFiles,
    rules: {
      'react-hooks/exhaustive-deps': 'error',
      'no-restricted-syntax': ['error', ...srcSelectors, ...wallClockSelectors],
    },
  },
  {
    files: ['src/hooks/use-accelerated-time.ts'],
    rules: { 'no-restricted-syntax': ['error', ...srcSelectors] },
  },
  {
    files: ['tests/**/*.{ts,tsx}'],
    rules: {
      // Playwright fixtures name their callback parameter `use`, which the hooks rule mistakes for React's use().
      'react-hooks/rules-of-hooks': 'off',
      'no-restricted-syntax': ['error', ...testSelectors, ...apiConstantSelectors],
    },
  },
  {
    files: ['tests/e2e/helpers/test-api.ts'],
    rules: { 'no-restricted-syntax': ['error', ...testSelectors] },
  },
]

export default eslintConfig
