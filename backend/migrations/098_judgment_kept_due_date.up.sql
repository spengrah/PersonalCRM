-- The date a review that keeps an open commitment's date keeps.
--
-- judgment_outcome.kept_due_date is the date the obligation of an outcome
-- suggesting a later date held when the user first reviewed that outcome,
-- before the review took effect: the date "Keep original" keeps. A first
-- review that accepts the later date moves the obligation, so a later review
-- that keeps the original date (a re-review, which changes nothing live) reads
-- the date it keeps here, and the bench shows the obligation's live date
-- beside it. NULL for every other outcome, and for an outcome reviewed before
-- this column existed, whose kept date is its obligation's current date.

ALTER TABLE judgment_outcome ADD COLUMN kept_due_date DATE;
