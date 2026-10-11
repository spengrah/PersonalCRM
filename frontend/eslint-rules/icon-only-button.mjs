// Flags <button>/<Button> with no accessible name: no aria-label, aria-labelledby, title
// or spread props, and content that renders elements but no text. Fragments and the
// branches of `cond ? a : b` and `cond && a` are followed; any other {expression} may
// render text, so it passes.
const NAME_ATTRS = new Set(['aria-label', 'aria-labelledby', 'title'])

function hasNameSource(opening) {
  return opening.attributes.some(
    attr => attr.type === 'JSXSpreadAttribute' || NAME_ATTRS.has(attr.name?.name)
  )
}

// The JSX a child can render: elements and fragments, through conditional branches.
function rendered(node) {
  if (!node) return []
  if (node.type === 'JSXExpressionContainer') return rendered(node.expression)
  if (node.type === 'ConditionalExpression')
    return [...rendered(node.consequent), ...rendered(node.alternate)]
  if (node.type === 'LogicalExpression' && node.operator === '&&') return rendered(node.right)
  return [node]
}

function mayRenderText(children) {
  return children.some(child =>
    rendered(child).some(node => {
      if (node.type === 'JSXText') return node.value.trim() !== ''
      if (node.type === 'JSXElement' || node.type === 'JSXFragment')
        return mayRenderText(node.children)
      if (node.type === 'Literal') return node.value !== null && typeof node.value !== 'boolean'
      return node.type !== 'JSXEmptyExpression'
    })
  )
}

function rendersElement(children) {
  return children.some(child =>
    rendered(child).some(
      node =>
        node.type === 'JSXElement' || (node.type === 'JSXFragment' && rendersElement(node.children))
    )
  )
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
        if (rendersElement(node.children) && !mayRenderText(node.children)) {
          context.report({ node: node.openingElement, messageId: 'iconOnly' })
        }
      },
    }
  },
}

export default rule
