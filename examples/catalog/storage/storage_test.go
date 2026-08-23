package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/storage"
	ddbsqlite "github.com/quells-bot/ddb-sqlite/pkg/ddb-sqlite"
)

// newRepo builds a repo over a fresh in-memory adapter and ensures the table
// exists. Every repo test starts from this state.
func newRepo(t *testing.T) storage.Repository {
	t.Helper()
	a, err := ddbsqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	r := storage.NewRepo(a)
	if err := r.EnsureTable(context.Background()); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}
	return r
}

func TestEnsureTableIdempotent(t *testing.T) {
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	r := storage.NewRepo(a)

	if err := r.EnsureTable(ctx); err != nil {
		t.Fatalf("first EnsureTable: %v", err)
	}
	if err := r.EnsureTable(ctx); err != nil {
		t.Fatalf("second EnsureTable: %v", err)
	}
}

func TestAuthorRoundtrip(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	_, err := r.GetAuthor(ctx, "alice")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetAuthor missing err = %v, want ErrNotFound", err)
	}

	a := storage.Author{ID: "alice", Name: "Alice", Bio: "wrote books"}
	if err := r.PutAuthor(ctx, &a); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}

	got, err := r.GetAuthor(ctx, "alice")
	if err != nil {
		t.Fatalf("GetAuthor: %v", err)
	}
	if got.ID != "alice" || got.Name != "Alice" || got.Bio != "wrote books" {
		t.Fatalf("got = %+v", got)
	}

	if err := r.DeleteAuthor(ctx, "alice"); err != nil {
		t.Fatalf("DeleteAuthor: %v", err)
	}
	_, err = r.GetAuthor(ctx, "alice")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetAuthor after delete err = %v, want ErrNotFound", err)
	}
}

func TestAuthorPutConflict(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	a := storage.Author{ID: "bob", Name: "Bob", Bio: ""}
	if err := r.PutAuthor(ctx, &a); err != nil {
		t.Fatalf("first PutAuthor: %v", err)
	}
	err := r.PutAuthor(ctx, &a)
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("second PutAuthor err = %v, want ErrConflict", err)
	}
}

func TestUpdateAuthor(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	a := storage.Author{ID: "carol", Name: "Carol", Bio: "old"}
	if err := r.PutAuthor(ctx, &a); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	if err := r.UpdateAuthor(ctx, "carol", "Carol Updated", "new bio"); err != nil {
		t.Fatalf("UpdateAuthor: %v", err)
	}
	got, err := r.GetAuthor(ctx, "carol")
	if err != nil {
		t.Fatalf("GetAuthor: %v", err)
	}
	if got.ID != "carol" || got.Name != "Carol Updated" || got.Bio != "new bio" {
		t.Fatalf("got = %+v", got)
	}
}

func TestListAuthors(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	for _, a := range []storage.Author{
		{ID: "a1", Name: "A", Bio: "x"},
		{ID: "a2", Name: "B", Bio: "y"},
	} {
		if err := r.PutAuthor(ctx, &a); err != nil {
			t.Fatalf("PutAuthor: %v", err)
		}
	}
	// A book under a1 must not appear as an author in ListAuthors.
	if err := r.PutBook(ctx, &storage.Book{ID: "b1", AuthorID: "a1", Title: "T", Year: 2000}); err != nil {
		t.Fatalf("PutBook: %v", err)
	}
	got, err := r.ListAuthors(ctx)
	if err != nil {
		t.Fatalf("ListAuthors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAuthors returned %d authors, want 2", len(got))
	}
}

func TestBookRoundtrip(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	if err := r.PutAuthor(ctx, &storage.Author{ID: "ann", Name: "Ann", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	b := storage.Book{ID: "bk1", AuthorID: "ann", Title: "T1", Year: 2001}
	if err := r.PutBook(ctx, &b); err != nil {
		t.Fatalf("PutBook: %v", err)
	}
	got, err := r.GetBook(ctx, "ann", "bk1")
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if got.ID != "bk1" || got.AuthorID != "ann" || got.Title != "T1" || got.Year != 2001 {
		t.Fatalf("got = %+v", got)
	}
	if err := r.DeleteBook(ctx, "ann", "bk1"); err != nil {
		t.Fatalf("DeleteBook: %v", err)
	}
	_, err = r.GetBook(ctx, "ann", "bk1")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetBook after delete err = %v, want ErrNotFound", err)
	}
}

func TestBookPutConflict(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	if err := r.PutAuthor(ctx, &storage.Author{ID: "ann", Name: "Ann", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	b := storage.Book{ID: "bk1", AuthorID: "ann", Title: "T1", Year: 2001}
	if err := r.PutBook(ctx, &b); err != nil {
		t.Fatalf("first PutBook: %v", err)
	}
	err := r.PutBook(ctx, &b)
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("second PutBook err = %v, want ErrConflict", err)
	}
}

func TestListBooksScopedToAuthor(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	for _, a := range []storage.Author{
		{ID: "x", Name: "X", Bio: ""},
		{ID: "y", Name: "Y", Bio: ""},
	} {
		if err := r.PutAuthor(ctx, &a); err != nil {
			t.Fatalf("PutAuthor: %v", err)
		}
	}
	books := []storage.Book{
		{ID: "b1", AuthorID: "x", Title: "T1", Year: 2000},
		{ID: "b2", AuthorID: "x", Title: "T2", Year: 2001},
		{ID: "b3", AuthorID: "y", Title: "T3", Year: 2002},
	}
	for _, b := range books {
		if err := r.PutBook(ctx, &b); err != nil {
			t.Fatalf("PutBook: %v", err)
		}
	}
	got, err := r.ListBooks(ctx, "x")
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListBooks(x) returned %d books, want 2", len(got))
	}
	// Only author x's books; b3 belongs to y and must not be returned.
	for _, b := range got {
		if b.AuthorID != "x" || b.ID == "b3" {
			t.Fatalf("unexpected book in x's list: %+v", b)
		}
	}
}

func TestUpdateBook(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	if err := r.PutAuthor(ctx, &storage.Author{ID: "ann", Name: "Ann", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	if err := r.PutBook(ctx, &storage.Book{ID: "bk1", AuthorID: "ann", Title: "old", Year: 2001}); err != nil {
		t.Fatalf("PutBook: %v", err)
	}
	if err := r.UpdateBook(ctx, "ann", "bk1", "new title", 2005); err != nil {
		t.Fatalf("UpdateBook: %v", err)
	}
	got, err := r.GetBook(ctx, "ann", "bk1")
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if got.Title != "new title" || got.Year != 2005 {
		t.Fatalf("got = %+v", got)
	}
}

func TestUpdateAuthorNotFound(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	err := r.UpdateAuthor(ctx, "missing", "Name", "bio")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("UpdateAuthor missing err = %v, want ErrNotFound", err)
	}
}

func TestDeleteAuthorNotFound(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	err := r.DeleteAuthor(ctx, "missing")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("DeleteAuthor missing err = %v, want ErrNotFound", err)
	}
}

func TestUpdateBookNotFound(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	if err := r.PutAuthor(ctx, &storage.Author{ID: "ann", Name: "Ann", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	err := r.UpdateBook(ctx, "ann", "missing", "title", 2001)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("UpdateBook missing err = %v, want ErrNotFound", err)
	}
}

func TestDeleteBookNotFound(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	if err := r.PutAuthor(ctx, &storage.Author{ID: "ann", Name: "Ann", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	err := r.DeleteBook(ctx, "ann", "missing")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("DeleteBook missing err = %v, want ErrNotFound", err)
	}
}
func TestSoftDeleteIsFiltered(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if err := r.PutAuthor(ctx, &storage.Author{ID: "del", Name: "Del", Bio: "x"}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	if err := r.DeleteAuthor(ctx, "del"); err != nil {
		t.Fatalf("DeleteAuthor: %v", err)
	}
	// Soft-deleted author must be filtered out by GetAuthor.
	if _, err := r.GetAuthor(ctx, "del"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetAuthor after soft-delete err = %v, want ErrNotFound", err)
	}
	// Soft-deleted item still occupies the PK — re-creating must conflict.
	if err := r.PutAuthor(ctx, &storage.Author{ID: "del", Name: "Del2", Bio: "y"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("PutAuthor after soft-delete err = %v, want ErrConflict (item still in table)", err)
	}
}
func TestListAuthorsExcludesSoftDeleted(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if err := r.PutAuthor(ctx, &storage.Author{ID: "a1", Name: "Alice", Bio: "x"}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	if err := r.PutAuthor(ctx, &storage.Author{ID: "a2", Name: "Bob", Bio: "y"}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	if err := r.DeleteAuthor(ctx, "a1"); err != nil {
		t.Fatalf("DeleteAuthor: %v", err)
	}

	got, err := r.ListAuthors(ctx)
	if err != nil {
		t.Fatalf("ListAuthors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListAuthors returned %d, want 1 (soft-deleted author excluded)", len(got))
	}
	if got[0].ID != "a2" {
		t.Fatalf("got = %+v, want a2", got[0])
	}
}
func TestListAllBooks(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if err := r.PutAuthor(ctx, &storage.Author{ID: "x", Name: "X", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	if err := r.PutAuthor(ctx, &storage.Author{ID: "y", Name: "Y", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	if err := r.PutBook(ctx, &storage.Book{ID: "b1", AuthorID: "x", Title: "T1", Year: 2000}); err != nil {
		t.Fatalf("PutBook: %v", err)
	}
	if err := r.PutBook(ctx, &storage.Book{ID: "b2", AuthorID: "y", Title: "T2", Year: 2001}); err != nil {
		t.Fatalf("PutBook: %v", err)
	}

	got, err := r.ListAllBooks(ctx)
	if err != nil {
		t.Fatalf("ListAllBooks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAllBooks returned %d, want 2", len(got))
	}
}
func TestSoftDeleteBooks(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if err := r.PutAuthor(ctx, &storage.Author{ID: "ann", Name: "Ann", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	books := []*storage.Book{
		{ID: "b1", AuthorID: "ann", Title: "T1", Year: 2000},
		{ID: "b2", AuthorID: "ann", Title: "T2", Year: 2001},
		{ID: "b3", AuthorID: "ann", Title: "T3", Year: 2002},
	}
	for _, b := range books {
		if err := r.PutBook(ctx, b); err != nil {
			t.Fatalf("PutBook: %v", err)
		}
	}

	if err := r.SoftDeleteBooks(ctx, books); err != nil {
		t.Fatalf("SoftDeleteBooks: %v", err)
	}

	for _, b := range books {
		if _, err := r.GetBook(ctx, b.AuthorID, b.ID); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("GetBook(%s) after SoftDeleteBooks err = %v, want ErrNotFound", b.ID, err)
		}
	}
}
func TestBatchDeleteBooksChunks(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if err := r.PutAuthor(ctx, &storage.Author{ID: "bulk", Name: "Bulk", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	var books []*storage.Book
	for i := range 30 {
		b := &storage.Book{
			ID:       fmt.Sprintf("bk%02d", i),
			AuthorID: "bulk",
			Title:    fmt.Sprintf("T%d", i),
			Year:     2000 + i,
		}
		if err := r.PutBook(ctx, b); err != nil {
			t.Fatalf("PutBook %d: %v", i, err)
		}
		books = append(books, b)
	}

	if err := r.SoftDeleteBooks(ctx, books); err != nil {
		t.Fatalf("SoftDeleteBooks: %v", err)
	}

	for _, b := range books {
		if _, err := r.GetBook(ctx, b.AuthorID, b.ID); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("GetBook(%s) after chunked batch err = %v, want ErrNotFound", b.ID, err)
		}
	}
}
