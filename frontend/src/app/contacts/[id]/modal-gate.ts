// Pure predicates for the contact-detail page's keyboard-handler modal gate.
// Kept OUT of page.tsx: Next.js App Router rejects any named export from a
// page file beyond its default component and a small reserved set
// (metadata, generateStaticParams, ...) — a build-time-only check that
// neither `tsc --noEmit` nor `next lint` catches.

// Whether the page's main (non-editing) body — and therefore every modal it
// hosts — is actually rendered. Three things independently unmount it, each
// with its own early return above this predicate's call site: still loading,
// no contact (TanStack Query keeps the previous successful `data` around
// during a background refetch that then errors, so `contact` alone can stay
// truthy after the page has already taken this branch), and edit mode (its
// own top-level return, entered via the underlying Edit button if a modal
// that doesn't trap focus lets Tab/Shift+Tab reach it — the modal's open
// flag then survives into a view that no longer renders it). Unit-tested
// directly so none of these races need reproducing live in E2E.
export function isContactMainRendered(state: {
  isLoading: boolean
  error: unknown
  contact: unknown
  isEditing: boolean
}): boolean {
  return !state.isLoading && !state.error && Boolean(state.contact) && !state.isEditing
}

// Whether the page-level keyboard handler should stand down because some
// modal it hosts is (or would be) open. Every flag here means nothing unless
// isMainRendered is also true — none of these modals can be mounted
// otherwise — so this is deliberately `mainRendered && (a || b || c)`, never
// `(mainRendered && a) || b`: that shape lets one flag bypass the gate on its
// own, which is exactly the regression this function's own unit test exists
// to catch by exercising the composition, not just isContactMainRendered.
export function isAnyDetailModalOpen(state: {
  isMainRendered: boolean
  isMergeModalOpen: boolean
  isLogModalOpen: boolean
  isAddTaskModalOpen: boolean
}): boolean {
  return (
    state.isMainRendered &&
    (state.isMergeModalOpen || state.isLogModalOpen || state.isAddTaskModalOpen)
  )
}
