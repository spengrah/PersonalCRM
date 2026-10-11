// Flags Playwright .locator() selectors that match on a CSS class (`.text-lg`,
// `div.border`, `main .card`, `[class*=...]`). Classes are styling details; tests
// locate by role, label, text or test id. Quoted strings and attribute brackets
// are removed first, so `[data-testid="a.b"]` and `a[href*="/x.json"]` pass.
// xpath= and text= selectors are not CSS and are skipped.

function selectorText(arg) {
  if (!arg) return undefined
  if (arg.type === 'Literal' && typeof arg.value === 'string') return arg.value
  if (arg.type === 'TemplateLiteral') return arg.quasis.map(q => q.value.cooked).join('')
  return undefined
}

function usesClass(selector) {
  if (/^\s*(xpath=|text=|\/\/)/.test(selector)) return false
  if (/\[\s*class\b/.test(selector)) return true
  const stripped = selector
    .replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g, '""')
    .replace(/\[[^\]]*\]/g, '[]')
  // A class token: a dot followed by an identifier start, not part of a number.
  return /(^|[^\w.])\.[a-zA-Z_-]|[a-zA-Z0-9_\])*]\.[a-zA-Z_-]/.test(stripped)
}

const rule = {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      classLocator: 'Locate by role, label, text or test id instead of a CSS class.',
    },
  },
  create(context) {
    return {
      CallExpression(node) {
        const callee = node.callee
        if (callee.type !== 'MemberExpression' || callee.property.name !== 'locator') return
        const selector = selectorText(node.arguments[0])
        if (selector !== undefined && usesClass(selector)) {
          context.report({ node: node.arguments[0], messageId: 'classLocator' })
        }
      },
    }
  },
}

export default rule
