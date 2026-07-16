// Minimal ambient declaration for the bun/node runtime bits the judge CLIs use
// (doctor.ts / report/render.ts run as entry scripts). Kept tiny so no
// @types/bun devDep is needed (design: no frontend/package.json change).

interface ImportMeta {
  // Bun sets `import.meta.main` true when the module is the entry script.
  main?: boolean
  // Node 20.11+/Bun expose the module's directory + file path.
  dirname?: string
  filename?: string
}
