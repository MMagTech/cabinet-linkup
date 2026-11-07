package standin

import "encoding/json"

// rawAnswer keeps an answer as it came, to read twice: once for an error, once for its fields.
type rawAnswer []byte

// UnmarshalJSON keeps the bytes.
func (r *rawAnswer) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}

func (r rawAnswer) into(v any) error { return json.Unmarshal(r, v) }

func jsonInto(b []byte, v any) error { return json.Unmarshal(b, v) }
