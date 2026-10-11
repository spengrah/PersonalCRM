// Flags direct queryClient cache invalidation (`invalidateQueries`, `removeQueries`,
// `resetQueries`, `refetchQueries`). src/lib/query-invalidation.ts maps each domain
// event to the keys it invalidates; components call invalidateFor(event) so that
// mapping stays the only place that knows which caches a write touches.

const CACHE_METHODS = new Set([
  'invalidateQueries',
  'removeQueries',
  'resetQueries',
  'refetchQueries',
])

const rule = {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      direct:
        'Call invalidateFor(DomainEvent) from src/lib/query-invalidation.ts instead of {{method}}.',
    },
  },
  create(context) {
    return {
      CallExpression(node) {
        const callee = node.callee
        if (callee.type !== 'MemberExpression') return
        const method =
          callee.property.type === 'Identifier' && !callee.computed
            ? callee.property.name
            : callee.property.value
        if (!CACHE_METHODS.has(method)) return
        context.report({ node: callee.property, messageId: 'direct', data: { method } })
      },
    }
  },
}

export default rule
