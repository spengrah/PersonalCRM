// Minimal ambient declarations for the bun-runtime bits the judge CLIs use
// (doctor.ts / label.ts / eval/run.ts / corpus/load.ts run under `bun run`).
// These are NOT exercised by the vitest suite (which imports only the pure
// modules), so keeping the surface tiny avoids adding @types/bun as a devDep
// (design: no frontend/package.json change).

declare namespace Bun {
  // Bun 1.2+ built-in YAML parser (used to load corpus *.yaml cases/labels).
  const YAML: {
    parse(input: string): unknown
    stringify(value: unknown): string
  }
}

interface ImportMeta {
  // Bun sets `import.meta.main` true when the module is the entry script.
  main?: boolean
  // Node 20.11+/Bun expose the module's directory + file path.
  dirname?: string
  filename?: string
}
