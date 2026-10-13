package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// PersonaDTO is the public persona shape. It contains only persona metadata and
// prompt text; database and runtime internals are kept out of the API contract.
type PersonaDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	IsActive bool   `json:"is_active"`
}

func personaDTO(p *db.Persona) PersonaDTO {
	return PersonaDTO{
		ID:       i64s(p.ID),
		Name:     p.Name,
		Prompt:   p.Prompt,
		IsActive: p.IsActive,
	}
}

func personaDTOs(in []*db.Persona) []PersonaDTO {
	out := make([]PersonaDTO, 0, len(in))
	for _, p := range in {
		out = append(out, personaDTO(p))
	}
	return out
}

// RegisterPersonaRoutes mounts the persona API on an existing mux. The caller
// decides where authentication middleware is applied, matching the other server
// route groups and allowing the main server wiring to remain independent.
func (s *Server) RegisterPersonaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/personas", s.pgListPersonas)
	mux.HandleFunc("POST /api/personas", s.pgSavePersona)
	mux.HandleFunc("POST /api/personas/active", s.pgActivatePersona)
	mux.HandleFunc("DELETE /api/personas/{id}", s.pgDeletePersona)
}

func writePersonaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrPersonaNotFound):
		writeErr(w, http.StatusNotFound, "persona not found")
	case errors.Is(err, db.ErrPersonaNameExists):
		writeErr(w, http.StatusConflict, "persona name already exists")
	default:
		// Keep database/driver details out of the response. In particular, this
		// module never exposes connection strings, API keys, or raw SQL errors.
		writeErr(w, http.StatusInternalServerError, "persona operation failed")
	}
}

func (s *Server) pgListPersonas(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	personas, err := pg.ListPersonas()
	if err != nil {
		writePersonaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"personas": personaDTOs(personas)})
}

func (s *Server) pgSavePersona(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ID < 0 {
		writeErr(w, http.StatusBadRequest, "persona id must be non-negative")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "persona name is required")
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "persona prompt is required")
		return
	}

	id, err := pg.SavePersona(&db.Persona{ID: body.ID, Name: name, Prompt: body.Prompt})
	if err != nil {
		writePersonaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) pgActivatePersona(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ID <= 0 {
		writeErr(w, http.StatusBadRequest, "persona id must be positive")
		return
	}
	if err := pg.SetActivePersonaContext(r.Context(), body.ID); err != nil {
		writePersonaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) pgDeletePersona(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, http.StatusBadRequest, "persona id must be positive")
		return
	}
	activeCleared, err := pg.DeletePersonaContext(r.Context(), id)
	if err != nil {
		writePersonaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":        id,
		"active_cleared": activeCleared,
	})
}
