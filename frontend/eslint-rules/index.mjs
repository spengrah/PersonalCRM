import apiClientOnly from './api-client-only.mjs'
import formControlPrimitives from './form-control-primitives.mjs'
import iconOnlyButton from './icon-only-button.mjs'
import noCssClassLocator from './no-css-class-locator.mjs'
import queryInvalidationRegistry from './query-invalidation-registry.mjs'

export default {
  rules: {
    'api-client-only': apiClientOnly,
    'form-control-primitives': formControlPrimitives,
    'icon-only-button': iconOnlyButton,
    'no-css-class-locator': noCssClassLocator,
    'query-invalidation-registry': queryInvalidationRegistry,
  },
}
