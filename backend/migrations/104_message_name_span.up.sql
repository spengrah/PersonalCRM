-- The name model's spans of a message's texts, the span store the masker reads
-- before it masks a request (G7 of .agents/spec/2026-10-21-masking-fixes.md).
--
-- A row is keyed by the message (its thread_content message_key), a SHA-256
-- digest of one of its texts and the model version (model, label and score
-- cutoff), and holds the offsets of the spans the model labels a person, as
-- UTF-8 byte ranges [starts[i], ends[i]) into that text. It holds no text: a
-- changed text has another digest, so it is asked again, and a changed model
-- version is another key. The backend is the only writer.
CREATE TABLE message_name_span (
    message_key TEXT NOT NULL,
    text_digest BYTEA NOT NULL CHECK (octet_length(text_digest) = 32),
    model_version TEXT NOT NULL CHECK (model_version <> ''),
    starts INTEGER[] NOT NULL,
    ends INTEGER[] NOT NULL CHECK (cardinality(ends) = cardinality(starts)),
    PRIMARY KEY (message_key, text_digest, model_version)
);
