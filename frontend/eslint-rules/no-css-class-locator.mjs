// Flags Playwright .locator() selectors that match on a CSS class (`.text-lg`,
// `div.border`, `main .card`, `[class*=...]`), including inside a `>>` chain
// (`text=Contact >> css=.card`). Classes are styling details; tests locate by role,
// label, text or test id. Each chain segment is checked on its own: segments in
// another engine (text=, xpath=, role=, ...) are skipped, and quoted strings are
// removed first, so attribute values such as `[title="[class]"]` or
// `[href*="/x.json"]` never count.

const OTHER_ENGINE =
  /^\s*(text|xpath|role|id|data-testid|data-test-id|data-test|internal:[\w-]+|nth|visible|_react|_vue)\s*=/
const XPATH = /^\s*(\/\/|\.\.)/

function selectorText(arg) {
  if (!arg) return undefined
  if (arg.type === 'Literal' && typeof arg.value === 'string') return arg.value
  if (arg.type === 'TemplateLiteral') return arg.quasis.map(q => q.value.cooked).join('')
  return undefined
}

// Blanks the inside of quoted strings, keeping the quotes and the length.
function withoutQuotedStrings(text) {
  return text.replace(
    /"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g,
    quoted => quoted[0] + ' '.repeat(quoted.length - 2) + quoted[0]
  )
}

function cssSegmentUsesClass(segment) {
  const css = withoutQuotedStrings(segment.replace(/^\s*css\s*=/, ''))
  if (/\[\s*class\b/.test(css)) return true
  const outsideBrackets = css.replace(/\[[^\]]*\]/g, '[]')
  // A class token: a dot followed by an identifier start, not part of a number.
  return /(^|[^\w.])\.[a-zA-Z_-]|[a-zA-Z0-9_\])*]\.[a-zA-Z_-]/.test(outsideBrackets)
}

function usesClass(selector) {
  // Split on `>>` outside quoted strings; quoted strings are blanked only for the split.
  const blanked = withoutQuotedStrings(selector)
  const segments = []
  let start = 0
  for (let i = blanked.indexOf('>>'); i !== -1; i = blanked.indexOf('>>', i + 2)) {
    segments.push(selector.slice(start, i))
    start = i + 2
  }
  segments.push(selector.slice(start))
  return segments.some(segment => {
    if (OTHER_ENGINE.test(segment) || XPATH.test(segment)) return false
    if (/^\s*["']/.test(segment)) return false // a quoted segment is a text selector
    return cssSegmentUsesClass(segment)
  })
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
