package app

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/bus"
)

type Controller interface {
	CreateAuthor(rw http.ResponseWriter, r *http.Request)
	GetAuthor(rw http.ResponseWriter, r *http.Request)
	ListAuthors(rw http.ResponseWriter, r *http.Request)
	UpdateAuthor(rw http.ResponseWriter, r *http.Request)
	DeleteAuthor(rw http.ResponseWriter, r *http.Request)
	CreateBook(rw http.ResponseWriter, r *http.Request)
	GetBook(rw http.ResponseWriter, r *http.Request)
	ListBooks(rw http.ResponseWriter, r *http.Request)
	ListAllBooks(rw http.ResponseWriter, r *http.Request)
	UpdateBook(rw http.ResponseWriter, r *http.Request)
	DeleteBook(rw http.ResponseWriter, r *http.Request)
}

var _ Controller = (*controller)(nil)

type controller struct {
	svc bus.Service
}

func NewController(svc bus.Service) *controller {
	return &controller{
		svc: svc,
	}
}

// errorResponse is the JSON body for every error response.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

// writeError maps a bus error to an HTTP status and JSON body.
func writeError(rw http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	msg := "internal"
	switch {
	case errors.Is(err, bus.ErrNotFound):
		status, msg = http.StatusNotFound, "not found"
	case errors.Is(err, bus.ErrConflict):
		status, msg = http.StatusConflict, "conflict"
	case errors.Is(err, bus.ErrValidation):
		status, msg = http.StatusBadRequest, err.Error()
	}
	writeJSON(rw, status, errorResponse{Error: msg})
}
