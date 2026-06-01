import '@testing-library/jest-dom'

// jsdom does not implement layout, so scrollIntoView is undefined. Components
// that scroll a deep-linked element into view (e.g. the Imports Interactions
// tab's ?session highlight) call it; stub it so those code paths run in tests.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {}
}
