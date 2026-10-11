// Flags calls to fetch and reads of NEXT_PUBLIC_API_KEY. src/lib/api-client.ts owns
// the API base URL, the key header, timeouts and the response envelope; other code
// calls it instead of building requests by hand.

const API_KEY = 'NEXT_PUBLIC_API_KEY'
const GLOBAL_OBJECTS = new Set(['window', 'globalThis', 'self'])

function propertyName(node) {
  if (node.type === 'Identifier') return node.name
  if (node.type === 'Literal') return node.value
  return undefined
}

// True when the identifier resolves to a binding the file declares, not the global.
function isLocal(context, identifier) {
  for (let scope = context.sourceCode.getScope(identifier); scope; scope = scope.upper) {
    const variable = scope.set.get(identifier.name)
    if (variable) return variable.defs.length > 0
  }
  return false
}

const rule = {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      fetch: 'Call the api client in src/lib/api-client.ts instead of fetch.',
      apiKey: `Only src/lib/api-client.ts reads ${API_KEY}; call the api client.`,
    },
  },
  create(context) {
    return {
      CallExpression(node) {
        const callee = node.callee
        const isGlobalFetch =
          callee.type === 'Identifier' && callee.name === 'fetch' && !isLocal(context, callee)
        const isMemberFetch =
          callee.type === 'MemberExpression' &&
          callee.object.type === 'Identifier' &&
          GLOBAL_OBJECTS.has(callee.object.name) &&
          propertyName(callee.property) === 'fetch'
        if (isGlobalFetch || isMemberFetch) context.report({ node: callee, messageId: 'fetch' })
      },
      MemberExpression(node) {
        if (propertyName(node.property) === API_KEY) {
          context.report({ node: node.property, messageId: 'apiKey' })
        }
      },
      // const { NEXT_PUBLIC_API_KEY } = process.env
      'ObjectPattern > Property'(node) {
        if (propertyName(node.key) === API_KEY)
          context.report({ node: node.key, messageId: 'apiKey' })
      },
    }
  },
}

export default rule
