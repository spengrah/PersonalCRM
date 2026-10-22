package threadctx

// CoverForTest returns c with every entry covered as Masker.Cover covers it,
// each text taking the name spans spans gives for it and every other text
// none, so a test can mask with the spans it chooses and no name model.
func CoverForTest(c Context, spans map[string][]NameSpan) Context {
	return covered(c, func(_, text string) ([]NameSpan, bool) { return spans[text], true })
}
