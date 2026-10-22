"""The GLiNER model the image holds, behind the interface the server reads.

MODEL_NAME is the one statement of which model the image holds: the image
build fetches it by this name, and the server refuses a request for any other.
"""

MODEL_NAME = "urchade/gliner_multi_pii-v1"


class GlinerModel:
    name = MODEL_NAME
    # GLiNER reads at most 384 words and its encoder at most 512 sub-word
    # tokens. 128 words keeps a window under both even when every word is an
    # emoji or a rare word of up to three sub-word tokens; 32 words of overlap
    # hold any name whole.
    window_words = 128
    overlap_words = 32

    def __init__(self):
        from gliner import GLiNER

        self._model = GLiNER.from_pretrained(MODEL_NAME)
        self._model.eval()

    def predict(self, text, label, threshold):
        """(start, end, score) per span, as character offsets into text."""
        entities = self._model.predict_entities(text, [label], threshold=threshold)
        return [(e["start"], e["end"], e["score"]) for e in entities]
