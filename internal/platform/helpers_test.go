package platform

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, v any) {
	json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, into any) {
	json.NewDecoder(r.Body).Decode(into)
}
