package app

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/bus"
)

type CreateAuthorRequest struct {
	Name string `json:"name"`
	Bio  string `json:"bio"`
}

type CreateAuthorResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Bio  string `json:"bio"`
}

func (c controller) CreateAuthor(rw http.ResponseWriter, r *http.Request) {
	var req CreateAuthorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, fmt.Errorf("invalid request body: %w", bus.ErrValidation))
		return
	}
	a, err := c.svc.CreateAuthor(r.Context(), req.Name, req.Bio)
	if err != nil {
		writeError(rw, err)
		return
	}
	rw.Header().Set("Location", "/authors/"+a.ID)
	writeJSON(rw, http.StatusCreated, CreateAuthorResponse{ID: a.ID, Name: a.Name, Bio: a.Bio})
}

type GetAuthorResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Bio  string `json:"bio"`
}

func (c controller) GetAuthor(rw http.ResponseWriter, r *http.Request) {
	a, err := c.svc.GetAuthor(r.Context(), r.PathValue("authorID"))
	if err != nil {
		writeError(rw, err)
		return
	}
	writeJSON(rw, http.StatusOK, GetAuthorResponse{ID: a.ID, Name: a.Name, Bio: a.Bio})
}

type ListAuthorsResponse struct {
	Authors []GetAuthorResponse `json:"authors"`
}

func (c controller) ListAuthors(rw http.ResponseWriter, r *http.Request) {
	authors, err := c.svc.ListAuthors(r.Context())
	if err != nil {
		writeError(rw, err)
		return
	}
	out := make([]GetAuthorResponse, 0, len(authors))
	for _, a := range authors {
		out = append(out, GetAuthorResponse{ID: a.ID, Name: a.Name, Bio: a.Bio})
	}
	writeJSON(rw, http.StatusOK, ListAuthorsResponse{Authors: out})
}

type UpdateAuthorRequest struct {
	Name string `json:"name"`
	Bio  string `json:"bio"`
}

type UpdateAuthorResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Bio  string `json:"bio"`
}

func (c controller) UpdateAuthor(rw http.ResponseWriter, r *http.Request) {
	var req UpdateAuthorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, fmt.Errorf("invalid request body: %w", bus.ErrValidation))
		return
	}
	a, err := c.svc.UpdateAuthor(r.Context(), r.PathValue("authorID"), req.Name, req.Bio)
	if err != nil {
		writeError(rw, err)
		return
	}
	writeJSON(rw, http.StatusOK, UpdateAuthorResponse{ID: a.ID, Name: a.Name, Bio: a.Bio})
}

func (c controller) DeleteAuthor(rw http.ResponseWriter, r *http.Request) {
	if err := c.svc.DeleteAuthor(r.Context(), r.PathValue("authorID")); err != nil {
		writeError(rw, err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}
