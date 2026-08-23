package app

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/bus"
)

type CreateBookRequest struct {
	Title string `json:"title"`
	Year  int    `json:"year"`
}

type CreateBookResponse struct {
	ID       string `json:"id"`
	AuthorID string `json:"authorID"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
}

func (c controller) CreateBook(rw http.ResponseWriter, r *http.Request) {
	var req CreateBookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, fmt.Errorf("invalid request body: %w", bus.ErrValidation))
		return
	}
	b, err := c.svc.CreateBook(r.Context(), r.PathValue("authorID"), req.Title, req.Year)
	if err != nil {
		writeError(rw, err)
		return
	}
	rw.Header().Set("Location", "/authors/"+b.AuthorID+"/books/"+b.ID)
	writeJSON(rw, http.StatusCreated, CreateBookResponse{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year})
}

type GetBookResponse struct {
	ID       string `json:"id"`
	AuthorID string `json:"authorID"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
}

func (c controller) GetBook(rw http.ResponseWriter, r *http.Request) {
	b, err := c.svc.GetBook(r.Context(), r.PathValue("authorID"), r.PathValue("bookID"))
	if err != nil {
		writeError(rw, err)
		return
	}
	writeJSON(rw, http.StatusOK, GetBookResponse{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year})
}

type ListBooksResponse struct {
	Books []GetBookResponse `json:"books"`
}

func (c controller) ListBooks(rw http.ResponseWriter, r *http.Request) {
	books, err := c.svc.ListBooks(r.Context(), r.PathValue("authorID"))
	if err != nil {
		writeError(rw, err)
		return
	}
	out := make([]GetBookResponse, 0, len(books))
	for _, b := range books {
		out = append(out, GetBookResponse{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year})
	}
	writeJSON(rw, http.StatusOK, ListBooksResponse{Books: out})
}

func (c controller) ListAllBooks(rw http.ResponseWriter, r *http.Request) {
	books, err := c.svc.ListAllBooks(r.Context())
	if err != nil {
		writeError(rw, err)
		return
	}
	out := make([]GetBookResponse, 0, len(books))
	for _, b := range books {
		out = append(out, GetBookResponse{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year})
	}
	writeJSON(rw, http.StatusOK, ListBooksResponse{Books: out})
}

type UpdateBookRequest struct {
	Title string `json:"title"`
	Year  int    `json:"year"`
}

type UpdateBookResponse struct {
	ID       string `json:"id"`
	AuthorID string `json:"authorID"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
}

func (c controller) UpdateBook(rw http.ResponseWriter, r *http.Request) {
	var req UpdateBookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, fmt.Errorf("invalid request body: %w", bus.ErrValidation))
		return
	}
	authorID := r.PathValue("authorID")
	bookID := r.PathValue("bookID")
	b, err := c.svc.UpdateBook(r.Context(), authorID, bookID, req.Title, req.Year)
	if err != nil {
		writeError(rw, err)
		return
	}
	writeJSON(rw, http.StatusOK, UpdateBookResponse{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year})
}

func (c controller) DeleteBook(rw http.ResponseWriter, r *http.Request) {
	authorID := r.PathValue("authorID")
	bookID := r.PathValue("bookID")
	if err := c.svc.DeleteBook(r.Context(), authorID, bookID); err != nil {
		writeError(rw, err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}
