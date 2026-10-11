// Flags raw <input>, <select> and <textarea> elements. The Input, Select and
// Textarea components in src/components/ui carry the shared styling, label and
// error wiring. Inputs whose type has no primitive (checkbox, radio, hidden, file)
// stay raw.

const CONTROLS = new Set(['input', 'select', 'textarea'])
const RAW_INPUT_TYPES = new Set(['checkbox', 'radio', 'hidden', 'file'])

function literalType(node) {
  const attr = node.attributes.find(
    a => a.type === 'JSXAttribute' && a.name.name === 'type' && a.value
  )
  if (!attr) return undefined
  if (attr.value.type === 'Literal') return attr.value.value
  const expr = attr.value.expression
  if (attr.value.type === 'JSXExpressionContainer' && expr.type === 'Literal') return expr.value
  return undefined
}

const rule = {
  meta: {
    type: 'suggestion',
    schema: [],
    messages: {
      raw: 'Use the {{primitive}} component from src/components/ui instead of a raw <{{element}}>.',
    },
  },
  create(context) {
    return {
      JSXOpeningElement(node) {
        if (node.name.type !== 'JSXIdentifier' || !CONTROLS.has(node.name.name)) return
        const element = node.name.name
        if (element === 'input' && RAW_INPUT_TYPES.has(literalType(node))) return
        const primitive = element[0].toUpperCase() + element.slice(1)
        context.report({ node: node.name, messageId: 'raw', data: { element, primitive } })
      },
    }
  },
}

export default rule
