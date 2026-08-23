package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newServer wires the full composition root against the in-memory adapter and
// returns a live HTTP server exercising the real routes, controller, service,
// and repository — no mocks anywhere in the stack.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("DDB_MOCK", "1")

	w, err := wireUp(context.Background())
	if err != nil {
		t.Fatalf("wireUp: %v", err)
	}
	t.Cleanup(func() {
		for _, f := range w.CleanUp {
			_ = f()
		}
	})

	mux := http.NewServeMux()
	routes(mux, w.App)
	return httptest.NewServer(mux)
}

// doJSON performs an HTTP request against the server, decoding the JSON
// response body into out (when non-nil). It returns the response for status
// and header assertions.
func doJSON(t *testing.T, method, url string, body any, out any) *http.Response {
	t.Helper()

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp
}

// TestEndToEnd drives the full REST surface against the in-memory adapter:
// create author → create book → list books → get book → update book →
// list all books → delete author → verify cascade (author + book gone).
func TestEndToEnd(t *testing.T) {
	srv := newServer(t)

	// Create author.
	var author struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Bio  string `json:"bio"`
	}
	resp := doJSON(t, http.MethodPost, srv.URL+"/authors",
		map[string]string{"name": "Kai Wells", "bio": "writes things"}, &author)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create author status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if author.ID == "" || author.Name != "Kai Wells" {
		t.Fatalf("unexpected author: %+v", author)
	}
	if loc := resp.Header.Get("Location"); loc != "/authors/"+author.ID {
		t.Fatalf("Location = %q, want %q", loc, "/authors/"+author.ID)
	}

	// Create book under the author.
	var book struct {
		ID       string `json:"id"`
		AuthorID string `json:"authorID"`
		Title    string `json:"title"`
		Year     int    `json:"year"`
	}
	resp = doJSON(t, http.MethodPost, srv.URL+"/authors/"+author.ID+"/books",
		map[string]any{"title": "The Catalog", "year": 2026}, &book)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create book status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if book.ID == "" || book.AuthorID != author.ID || book.Title != "The Catalog" || book.Year != 2026 {
		t.Fatalf("unexpected book: %+v", book)
	}

	// List books by author.
	var list struct {
		Books []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"books"`
	}
	resp = doJSON(t, http.MethodGet, srv.URL+"/authors/"+author.ID+"/books", nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list books status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if len(list.Books) != 1 || list.Books[0].ID != book.ID {
		t.Fatalf("unexpected books: %+v", list.Books)
	}

	// Get book.
	var got struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Year  int    `json:"year"`
	}
	resp = doJSON(t, http.MethodGet, srv.URL+"/authors/"+author.ID+"/books/"+book.ID, nil, &got)
	if resp.StatusCode != http.StatusOK || got.ID != book.ID || got.Title != "The Catalog" {
		t.Fatalf("get book: status=%d body=%+v", resp.StatusCode, got)
	}

	// Update book.
	var updated struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
	}
	resp = doJSON(t, http.MethodPut, srv.URL+"/authors/"+author.ID+"/books/"+book.ID,
		map[string]any{"title": "The Catalog, Revised", "year": 2027}, &updated)
	if resp.StatusCode != http.StatusOK || updated.Title != "The Catalog, Revised" || updated.Year != 2027 {
		t.Fatalf("update book: status=%d body=%+v", resp.StatusCode, updated)
	}

	// List all books (GET /books) — the new GSI-backed endpoint.
	var allBooks struct {
		Books []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"books"`
	}
	resp = doJSON(t, http.MethodGet, srv.URL+"/books", nil, &allBooks)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list all books status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if len(allBooks.Books) != 1 || allBooks.Books[0].ID != book.ID {
		t.Fatalf("unexpected all books: %+v", allBooks.Books)
	}

	// Delete author — cascades to soft-delete the book.
	resp = doJSON(t, http.MethodDelete, srv.URL+"/authors/"+author.ID, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete author status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	// Author is gone.
	resp = doJSON(t, http.MethodGet, srv.URL+"/authors/"+author.ID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted author status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	// Book is gone (cascade delete).
	resp = doJSON(t, http.MethodGet, srv.URL+"/authors/"+author.ID+"/books/"+book.ID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get cascaded book status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	// GET /books no longer returns the cascaded book.
	resp = doJSON(t, http.MethodGet, srv.URL+"/books", nil, &allBooks)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list all books after cascade status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if len(allBooks.Books) != 0 {
		t.Fatalf("expected 0 books after cascade, got %d", len(allBooks.Books))
	}
}
