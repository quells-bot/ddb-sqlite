package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/app"
	"github.com/quells-bot/ddb-sqlite/examples/catalog/bus"
	"github.com/quells-bot/ddb-sqlite/examples/catalog/storage"
	ddbsqlite "github.com/quells-bot/ddb-sqlite/pkg/ddb-sqlite"
)

// testApp bundles the controller under test with the service used to seed data.
type testApp struct {
	ctrl app.Controller
	svc  bus.Service
}

// newTestApp wires a real storage repository (backed by an in-memory ddbsqlite
// adapter) through the real bus service into the real controller. No mocks.
func newTestApp(t *testing.T) testApp {
	t.Helper()
	a, err := ddbsqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	repo := storage.NewRepo(a)
	if err := repo.EnsureTable(context.Background()); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}
	svc := bus.NewService(repo)
	return testApp{ctrl: app.NewController(svc), svc: svc}
}

// do invokes a controller method with the given path values set, returning the
// response recorder.
func do(t *testing.T, h func(http.ResponseWriter, *http.Request), method, path string, body io.Reader, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rw := httptest.NewRecorder()
	h(rw, req)
	return rw
}

// decodeError decodes the JSON error body and returns its "error" field.
func decodeError(t *testing.T, rw *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return got.Error
}

// seedAuthor creates an author through the service and returns its ID.
func seedAuthor(t *testing.T, svc bus.Service, name, bio string) string {
	t.Helper()
	a, err := svc.CreateAuthor(context.Background(), name, bio)
	if err != nil {
		t.Fatalf("CreateAuthor: %v", err)
	}
	return a.ID
}

// seedBook creates a book through the service and returns its ID.
func seedBook(t *testing.T, svc bus.Service, authorID, title string, year int) string {
	t.Helper()
	b, err := svc.CreateBook(context.Background(), authorID, title, year)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	return b.ID
}

func TestGetAuthor(t *testing.T) {
	ta := newTestApp(t)
	id := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.GetAuthor, http.MethodGet, "/authors/"+id, nil, map[string]string{"authorID": id})

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.GetAuthorResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := app.GetAuthorResponse{ID: id, Name: "Ada Lovelace", Bio: "pioneer"}
	if got != want {
		t.Fatalf("body = %+v, want %+v", got, want)
	}
}

func TestGetAuthorNotFound(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.GetAuthor, http.MethodGet, "/authors/missing", nil, map[string]string{"authorID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}

func TestListAuthors(t *testing.T) {
	ta := newTestApp(t)
	seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")
	seedAuthor(t, ta.svc, "Grace Hopper", "compiler")

	rw := do(t, ta.ctrl.ListAuthors, http.MethodGet, "/authors", nil, nil)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.ListAuthorsResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Authors) != 2 {
		t.Fatalf("len(Authors) = %d, want 2", len(got.Authors))
	}
}

func TestListAuthorsEmpty(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.ListAuthors, http.MethodGet, "/authors", nil, nil)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.ListAuthorsResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Authors) != 0 {
		t.Fatalf("len(Authors) = %d, want 0", len(got.Authors))
	}
}

func TestCreateAuthor(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.CreateAuthor, http.MethodPost, "/authors", strings.NewReader(`{"name":"Ada Lovelace","bio":"pioneer"}`), nil)

	if rw.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusCreated)
	}
	var got app.CreateAuthorResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if got.Name != "Ada Lovelace" || got.Bio != "pioneer" {
		t.Fatalf("body = %+v", got)
	}
	if loc := rw.Header().Get("Location"); loc != "/authors/"+got.ID {
		t.Fatalf("Location = %q, want %q", loc, "/authors/"+got.ID)
	}
}

func TestCreateAuthorValidation(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.CreateAuthor, http.MethodPost, "/authors", strings.NewReader(`{"name":"","bio":"x"}`), nil)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusBadRequest)
	}
	if got := decodeError(t, rw); got != "name must not be empty: validation failed" {
		t.Fatalf("error = %q", got)
	}
}

func TestCreateAuthorMalformedJSON(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.CreateAuthor, http.MethodPost, "/authors", strings.NewReader(`{not json`), nil)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusBadRequest)
	}
	if got := decodeError(t, rw); got != "invalid request body: validation failed" {
		t.Fatalf("error = %q", got)
	}
}
func TestUpdateAuthor(t *testing.T) {
	ta := newTestApp(t)
	id := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.UpdateAuthor, http.MethodPut, "/authors/"+id, strings.NewReader(`{"name":"Ada King","bio":"countess"}`), map[string]string{"authorID": id})

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.UpdateAuthorResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := app.UpdateAuthorResponse{ID: id, Name: "Ada King", Bio: "countess"}
	if got != want {
		t.Fatalf("body = %+v, want %+v", got, want)
	}
}

func TestUpdateAuthorNotFound(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.UpdateAuthor, http.MethodPut, "/authors/missing", strings.NewReader(`{"name":"Ada","bio":"x"}`), map[string]string{"authorID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}

func TestUpdateAuthorValidation(t *testing.T) {
	ta := newTestApp(t)
	id := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.UpdateAuthor, http.MethodPut, "/authors/"+id, strings.NewReader(`{"name":"","bio":"x"}`), map[string]string{"authorID": id})

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusBadRequest)
	}
	if got := decodeError(t, rw); got != "name must not be empty: validation failed" {
		t.Fatalf("error = %q", got)
	}
}

func TestDeleteAuthor(t *testing.T) {
	ta := newTestApp(t)
	id := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.DeleteAuthor, http.MethodDelete, "/authors/"+id, nil, map[string]string{"authorID": id})

	if rw.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNoContent)
	}
	if rw.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rw.Body.String())
	}
}

func TestDeleteAuthorNotFound(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.DeleteAuthor, http.MethodDelete, "/authors/missing", nil, map[string]string{"authorID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}

func TestGetBook(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")
	bookID := seedBook(t, ta.svc, authorID, "Notes", 1843)

	rw := do(t, ta.ctrl.GetBook, http.MethodGet, "/authors/"+authorID+"/books/"+bookID, nil, map[string]string{"authorID": authorID, "bookID": bookID})

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.GetBookResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := app.GetBookResponse{ID: bookID, AuthorID: authorID, Title: "Notes", Year: 1843}
	if got != want {
		t.Fatalf("body = %+v, want %+v", got, want)
	}
}

func TestGetBookNotFound(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.GetBook, http.MethodGet, "/authors/"+authorID+"/books/missing", nil, map[string]string{"authorID": authorID, "bookID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}

func TestListBooks(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")
	seedBook(t, ta.svc, authorID, "Notes", 1843)
	seedBook(t, ta.svc, authorID, "Sketch", 1842)

	rw := do(t, ta.ctrl.ListBooks, http.MethodGet, "/authors/"+authorID+"/books", nil, map[string]string{"authorID": authorID})

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.ListBooksResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Books) != 2 {
		t.Fatalf("len(Books) = %d, want 2", len(got.Books))
	}
}

func TestListBooksAuthorMissing(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.ListBooks, http.MethodGet, "/authors/missing/books", nil, map[string]string{"authorID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}

func TestCreateBook(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.CreateBook, http.MethodPost, "/authors/"+authorID+"/books", strings.NewReader(`{"title":"Notes","year":1843}`), map[string]string{"authorID": authorID})

	if rw.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusCreated)
	}
	var got app.CreateBookResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if got.AuthorID != authorID || got.Title != "Notes" || got.Year != 1843 {
		t.Fatalf("body = %+v", got)
	}
	wantLoc := "/authors/" + authorID + "/books/" + got.ID
	if loc := rw.Header().Get("Location"); loc != wantLoc {
		t.Fatalf("Location = %q, want %q", loc, wantLoc)
	}
}

func TestCreateBookValidation(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.CreateBook, http.MethodPost, "/authors/"+authorID+"/books", strings.NewReader(`{"title":"","year":1843}`), map[string]string{"authorID": authorID})

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusBadRequest)
	}
	if got := decodeError(t, rw); got != "title must not be empty: validation failed" {
		t.Fatalf("error = %q", got)
	}
}

func TestCreateBookValidationYear(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.CreateBook, http.MethodPost, "/authors/"+authorID+"/books", strings.NewReader(`{"title":"Notes","year":0}`), map[string]string{"authorID": authorID})

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusBadRequest)
	}
	if got := decodeError(t, rw); got != "year must be positive: validation failed" {
		t.Fatalf("error = %q", got)
	}
}

func TestCreateBookAuthorMissing(t *testing.T) {
	ta := newTestApp(t)

	rw := do(t, ta.ctrl.CreateBook, http.MethodPost, "/authors/missing/books", strings.NewReader(`{"title":"Notes","year":1843}`), map[string]string{"authorID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}

func TestUpdateBook(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")
	bookID := seedBook(t, ta.svc, authorID, "Notes", 1843)

	rw := do(t, ta.ctrl.UpdateBook, http.MethodPut, "/authors/"+authorID+"/books/"+bookID, strings.NewReader(`{"title":"Sketch","year":1842}`), map[string]string{"authorID": authorID, "bookID": bookID})

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.UpdateBookResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := app.UpdateBookResponse{ID: bookID, AuthorID: authorID, Title: "Sketch", Year: 1842}
	if got != want {
		t.Fatalf("body = %+v, want %+v", got, want)
	}
}

func TestUpdateBookNotFound(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.UpdateBook, http.MethodPut, "/authors/"+authorID+"/books/missing", strings.NewReader(`{"title":"Sketch","year":1842}`), map[string]string{"authorID": authorID, "bookID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}

func TestUpdateBookValidation(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")
	bookID := seedBook(t, ta.svc, authorID, "Notes", 1843)

	rw := do(t, ta.ctrl.UpdateBook, http.MethodPut, "/authors/"+authorID+"/books/"+bookID, strings.NewReader(`{"title":"","year":1842}`), map[string]string{"authorID": authorID, "bookID": bookID})

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusBadRequest)
	}
	if got := decodeError(t, rw); got != "title must not be empty: validation failed" {
		t.Fatalf("error = %q", got)
	}
}

func TestDeleteBook(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")
	bookID := seedBook(t, ta.svc, authorID, "Notes", 1843)

	rw := do(t, ta.ctrl.DeleteBook, http.MethodDelete, "/authors/"+authorID+"/books/"+bookID, nil, map[string]string{"authorID": authorID, "bookID": bookID})

	if rw.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNoContent)
	}
	if rw.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rw.Body.String())
	}
}

func TestDeleteBookNotFound(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")

	rw := do(t, ta.ctrl.DeleteBook, http.MethodDelete, "/authors/"+authorID+"/books/missing", nil, map[string]string{"authorID": authorID, "bookID": "missing"})

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rw); got != "not found" {
		t.Fatalf("error = %q, want %q", got, "not found")
	}
}
func TestListAllBooks(t *testing.T) {
	ta := newTestApp(t)
	authorID := seedAuthor(t, ta.svc, "Ada Lovelace", "pioneer")
	otherID := seedAuthor(t, ta.svc, "Grace Hopper", "compiler")
	seedBook(t, ta.svc, authorID, "Notes", 1843)
	seedBook(t, ta.svc, otherID, "COBOL", 1959)

	rw := do(t, ta.ctrl.ListAllBooks, http.MethodGet, "/books", nil, nil)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusOK)
	}
	var got app.ListBooksResponse
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Books) != 2 {
		t.Fatalf("len(Books) = %d, want 2", len(got.Books))
	}
}

// conflictService forces bus.ErrConflict from CreateAuthor/CreateBook while
// delegating every other method to a real service. It exercises the app
// layer's conflict mapping, which is otherwise unreachable because the bus
// layer generates random IDs that never collide.
type conflictService struct {
	bus.Service
}

func (c conflictService) CreateAuthor(ctx context.Context, name, bio string) (*bus.Author, error) {
	return nil, bus.ErrConflict
}

func (c conflictService) CreateBook(ctx context.Context, authorID, title string, year int) (*bus.Book, error) {
	return nil, bus.ErrConflict
}

// errorService forces a generic (non-sentinel) error from GetAuthor while
// delegating every other method to a real service. It exercises the app
// layer's 500 mapping, which is otherwise unreachable with a real repository.
type errorService struct {
	bus.Service
}

func (e errorService) GetAuthor(ctx context.Context, id string) (*bus.Author, error) {
	return nil, errors.New("boom")
}

func TestCreateAuthorConflict(t *testing.T) {
	ta := newTestApp(t)
	ctrl := app.NewController(conflictService{Service: ta.svc})

	rw := do(t, ctrl.CreateAuthor, http.MethodPost, "/authors", strings.NewReader(`{"name":"Ada","bio":"x"}`), nil)

	if rw.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusConflict)
	}
	if got := decodeError(t, rw); got != "conflict" {
		t.Fatalf("error = %q, want %q", got, "conflict")
	}
}

func TestGetAuthorInternalError(t *testing.T) {
	ta := newTestApp(t)
	ctrl := app.NewController(errorService{Service: ta.svc})

	rw := do(t, ctrl.GetAuthor, http.MethodGet, "/authors/x", nil, map[string]string{"authorID": "x"})

	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rw.Code, http.StatusInternalServerError)
	}
	if got := decodeError(t, rw); got != "internal" {
		t.Fatalf("error = %q, want %q", got, "internal")
	}
}
