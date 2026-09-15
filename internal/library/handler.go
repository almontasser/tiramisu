package library

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxBodyBytes = 1 << 20

// Handler exposes the manager over HTTP: POST /api/library/add,
// POST /api/library/remove and GET /api/library/list. It exists so a client with no
// access to the filesystem can still file a title into the library.
type Handler struct {
	mgr *Manager
}

func NewHandler(m *Manager) *Handler { return &Handler{mgr: m} }

func (h *Handler) Add(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req AddRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.mgr.Add(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	status := http.StatusCreated
	if resp.AlreadyPresent {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}

func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req RemoveRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.mgr.Remove(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Missing serves GET /api/library/missing: every series compared with the
// episodes TMDB says have aired. type is tv, anime, or all. refresh=1 drops
// the cached TMDB answers first, which makes that one request slow.
func (h *Handler) Missing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	if q.Get("refresh") == "1" {
		h.mgr.ResetMissingCache()
	}
	rep, err := h.mgr.Missing(r.Context(), q.Get("type"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// Inspect serves POST /api/library/inspect: what is inside a torrent, and how
// it looks like it should be filed. Nothing is written.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req InspectRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed body")
		return
	}
	resp, err := h.mgr.Inspect(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ListPage is the paged form of a library listing. The bare array returned by
// List is kept for existing callers; anything with a UI wants a window plus the
// total, or it ends up shipping every episode in one response.
type ListPage struct {
	Items  []Item `json:"items"`
	Total  int    `json:"total"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// List serves GET /api/library/list.
//
// Without limit it answers exactly as before: the plain array for one kind.
// With limit (or type=all, or a search term) it answers a ListPage instead, so
// a browser can page through 6000+ episodes rather than load them at once.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	// type=gaps reuses this endpoint rather than adding one: the response stays an
	// array, so a client that asks for movies or tv sees no change.
	if strings.EqualFold(r.URL.Query().Get("type"), "gaps") {
		gaps, total, err := h.mgr.ListGaps()
		if err != nil {
			writeAPIError(w, err)
			return
		}
		// The body stays an array, so the count travels in a header: without it a
		// client reading a capped page cannot tell there is more behind it.
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		writeJSON(w, http.StatusOK, gaps)
		return
	}

	q := r.URL.Query()
	kind := q.Get("type")
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))
	limit := atoiDefault(q.Get("limit"), 0)
	offset := atoiDefault(q.Get("offset"), 0)

	// "all" spans the three trees; anything else keeps List's own normalisation,
	// including the historical default of "movie" for an empty type.
	kinds := []string{kind}
	paged := limit > 0 || search != ""
	if strings.EqualFold(strings.TrimSpace(kind), "all") {
		kinds = []string{"movie", "tv", "anime"}
		paged = true
	}

	items := []Item{}
	for _, k := range kinds {
		part, err := h.mgr.List(k)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		items = append(items, part...)
	}

	if !paged {
		writeJSON(w, http.StatusOK, items)
		return
	}

	if search != "" {
		kept := items[:0:0]
		for _, it := range items {
			// Match the filename and the infohash: the name is what a person
			// reads, the hash is what they have when chasing a specific torrent.
			if strings.Contains(strings.ToLower(it.FusePath), search) ||
				strings.Contains(strings.ToLower(it.Hash), search) {
				kept = append(kept, it)
			}
		}
		items = kept
	}

	total := len(items)
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	writeJSON(w, http.StatusOK, ListPage{
		Items: items[offset:end], Total: total, Offset: offset, Limit: limit,
	})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func decode(r *http.Request, dst interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeAPIError(w http.ResponseWriter, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.Status, apiErr.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
