// Flags <button>/<Button> with no accessible name: no aria-label, aria-labelledby, title or
// spread props, and children that are only elements with no text and no {expressions}.
const NAME_ATTRS = new Set(['aria-label', 'aria-labelledby', 'title'])

function hasNameSource(opening) {
  return opening.attributes.some(
    attr => attr.type === 'JSXSpreadAttribute' || NAME_ATTRS.has(attr.name?.name)
  )
}

// True when the children could render visible text: literal text or any {expression}.
function mayRenderText(children) {
  return children.some(child => {
    if (child.type === 'JSXText') return child.value.trim() !== ''
    if (child.type === 'JSXElement' || child.type === 'JSXFragment')
      return mayRenderText(child.children)
    return child.type === 'JSXExpressionContainer' && child.expression.type !== 'JSXEmptyExpression'
  })
}

const rule = {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      iconOnly: 'Icon-only button needs an aria-label, aria-labelledby or title.',
    },
  },
  create(context) {
    return {
      JSXElement(node) {
        const name = node.openingElement.name
        if (name.type !== 'JSXIdentifier' || (name.name !== 'button' && name.name !== 'Button')) {
          return
        }
        if (hasNameSource(node.openingElement)) return
        const hasChildElement = node.children.some(c => c.type === 'JSXElement')
        if (hasChildElement && !mayRenderText(node.children)) {
          context.report({ node: node.openingElement, messageId: 'iconOnly' })
        }
      },
    }
  },
}

export default { rules: { 'icon-only-button': rule } }
