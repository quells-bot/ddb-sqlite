package bus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/bus"
	"github.com/quells-bot/ddb-sqlite/examples/catalog/storage"
	ddbsqlite "github.com/quells-bot/ddb-sqlite/pkg/ddb-sqlite"
)

// newRepo builds a real storage.Repository over a fresh in-memory ddbsqlite
// adapter and ensures the table exists. Every bus test starts from this state.
func newRepo(t *testing.T) storage.Repository {
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
	return repo
}

// conflictRepo forces storage.ErrConflict from PutAuthor/PutBook while
// delegating every other method to a real repository. It exercises the bus
// layer's conflict mapping, which is otherwise unreachable because CreateAuthor
// and CreateBook generate random IDs that never collide.
type conflictRepo struct {
	storage.Repository
}

func (c conflictRepo) PutAuthor(ctx context.Context, a *storage.Author) error {
	return storage.ErrConflict
}

func (c conflictRepo) PutBook(ctx context.Context, b *storage.Book) error {
	return storage.ErrConflict
}

func TestCreateAuthor(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	a, err := svc.CreateAuthor(ctx, "Jane Austen", "English novelist")
	if err != nil {
		t.Fatalf("CreateAuthor: %v", err)
	}
	if a.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if a.Name != "Jane Austen" || a.Bio != "English novelist" {
		t.Fatalf("unexpected author: %+v", a)
	}

	got, err := svc.GetAuthor(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAuthor: %v", err)
	}
	if got.ID != a.ID || got.Name != a.Name || got.Bio != a.Bio {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", got, a)
	}
}

func TestCreateAuthorValidation(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	_, err := svc.CreateAuthor(ctx, "", "bio")
	if !errors.Is(err, bus.ErrValidation) {
		t.Fatalf("CreateAuthor: got %v, want ErrValidation", err)
	}
}

func TestGetAuthorNotFound(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	_, err := svc.GetAuthor(ctx, "missing")
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("GetAuthor: got %v, want ErrNotFound", err)
	}
}

func TestUpdateAuthor(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	a, err := svc.CreateAuthor(ctx, "Jane Austen", "old bio")
	if err != nil {
		t.Fatalf("CreateAuthor: %v", err)
	}

	updated, err := svc.UpdateAuthor(ctx, a.ID, "Jane Austen", "new bio")
	if err != nil {
		t.Fatalf("UpdateAuthor: %v", err)
	}
	if updated.Name != "Jane Austen" || updated.Bio != "new bio" {
		t.Fatalf("unexpected updated author: %+v", updated)
	}

	got, err := svc.GetAuthor(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAuthor: %v", err)
	}
	if got.Bio != "new bio" {
		t.Fatalf("persisted bio = %q, want %q", got.Bio, "new bio")
	}
}

func TestUpdateAuthorValidation(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	a, err := svc.CreateAuthor(ctx, "Jane Austen", "bio")
	if err != nil {
		t.Fatalf("CreateAuthor: %v", err)
	}

	_, err = svc.UpdateAuthor(ctx, a.ID, "", "bio")
	if !errors.Is(err, bus.ErrValidation) {
		t.Fatalf("UpdateAuthor: got %v, want ErrValidation", err)
	}
}

func TestDeleteAuthor(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	a, err := svc.CreateAuthor(ctx, "Jane Austen", "bio")
	if err != nil {
		t.Fatalf("CreateAuthor: %v", err)
	}

	if err := svc.DeleteAuthor(ctx, a.ID); err != nil {
		t.Fatalf("DeleteAuthor: %v", err)
	}

	if _, err := svc.GetAuthor(ctx, a.ID); !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("GetAuthor after delete: got %v, want ErrNotFound", err)
	}
}

func TestListAuthors(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	if _, err := svc.CreateAuthor(ctx, "Jane Austen", "bio 1"); err != nil {
		t.Fatalf("CreateAuthor: %v", err)
	}
	if _, err := svc.CreateAuthor(ctx, "Charles Dickens", "bio 2"); err != nil {
		t.Fatalf("CreateAuthor: %v", err)
	}

	authors, err := svc.ListAuthors(ctx)
	if err != nil {
		t.Fatalf("ListAuthors: %v", err)
	}
	if len(authors) != 2 {
		t.Fatalf("ListAuthors: got %d authors, want 2", len(authors))
	}
	names := map[string]bool{}
	for _, a := range authors {
		names[a.Name] = true
	}
	if !names["Jane Austen"] || !names["Charles Dickens"] {
		t.Fatalf("ListAuthors: missing expected names, got %v", names)
	}
}

func TestCreateAuthorConflict(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(conflictRepo{Repository: newRepo(t)})

	_, err := svc.CreateAuthor(ctx, "Jane Austen", "bio")
	if !errors.Is(err, bus.ErrConflict) {
		t.Fatalf("CreateAuthor: got %v, want ErrConflict", err)
	}
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

func TestCreateBook(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	b, err := svc.CreateBook(ctx, authorID, "Emma", 1815)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if b.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if b.AuthorID != authorID || b.Title != "Emma" || b.Year != 1815 {
		t.Fatalf("unexpected book: %+v", b)
	}

	got, err := svc.GetBook(ctx, authorID, b.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if got.ID != b.ID || got.AuthorID != authorID || got.Title != "Emma" || got.Year != 1815 {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", got, b)
	}
}

func TestCreateBookValidation(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	if _, err := svc.CreateBook(ctx, authorID, "", 1815); !errors.Is(err, bus.ErrValidation) {
		t.Fatalf("empty title: got %v, want ErrValidation", err)
	}
	if _, err := svc.CreateBook(ctx, authorID, "Emma", 0); !errors.Is(err, bus.ErrValidation) {
		t.Fatalf("zero year: got %v, want ErrValidation", err)
	}
}

func TestCreateBookAuthorMissing(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	_, err := svc.CreateBook(ctx, "missing", "Emma", 1815)
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("CreateBook: got %v, want ErrNotFound", err)
	}
}

func TestGetBookNotFound(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	_, err := svc.GetBook(ctx, authorID, "missing")
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("GetBook: got %v, want ErrNotFound", err)
	}
}

func TestUpdateBook(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	b, err := svc.CreateBook(ctx, authorID, "Emma", 1815)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	updated, err := svc.UpdateBook(ctx, authorID, b.ID, "Emma (revised)", 1816)
	if err != nil {
		t.Fatalf("UpdateBook: %v", err)
	}
	if updated.Title != "Emma (revised)" || updated.Year != 1816 {
		t.Fatalf("unexpected updated book: %+v", updated)
	}

	got, err := svc.GetBook(ctx, authorID, b.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if got.Title != "Emma (revised)" || got.Year != 1816 {
		t.Fatalf("persisted book = %+v, want title %q year %d", got, "Emma (revised)", 1816)
	}
}

func TestUpdateBookValidation(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	b, err := svc.CreateBook(ctx, authorID, "Emma", 1815)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	if _, err := svc.UpdateBook(ctx, authorID, b.ID, "", 1815); !errors.Is(err, bus.ErrValidation) {
		t.Fatalf("empty title: got %v, want ErrValidation", err)
	}
	if _, err := svc.UpdateBook(ctx, authorID, b.ID, "Emma", 0); !errors.Is(err, bus.ErrValidation) {
		t.Fatalf("zero year: got %v, want ErrValidation", err)
	}
}

func TestDeleteBook(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	b, err := svc.CreateBook(ctx, authorID, "Emma", 1815)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	if err := svc.DeleteBook(ctx, authorID, b.ID); err != nil {
		t.Fatalf("DeleteBook: %v", err)
	}

	if _, err := svc.GetBook(ctx, authorID, b.ID); !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("GetBook after delete: got %v, want ErrNotFound", err)
	}
}

func TestListBooks(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")
	otherID := seedAuthor(t, svc, "Charles Dickens", "bio")

	if _, err := svc.CreateBook(ctx, authorID, "Emma", 1815); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := svc.CreateBook(ctx, authorID, "Pride and Prejudice", 1813); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := svc.CreateBook(ctx, otherID, "Oliver Twist", 1838); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	books, err := svc.ListBooks(ctx, authorID)
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("ListBooks: got %d books, want 2", len(books))
	}
	for _, b := range books {
		if b.AuthorID != authorID {
			t.Fatalf("ListBooks: book %+v leaked from another author", b)
		}
	}
}

func TestListBooksAuthorMissing(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	_, err := svc.ListBooks(ctx, "missing")
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("ListBooks: got %v, want ErrNotFound", err)
	}
}

func TestCreateBookConflict(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	if err := repo.PutAuthor(ctx, &storage.Author{ID: "a1", Name: "Jane Austen", Bio: ""}); err != nil {
		t.Fatalf("PutAuthor: %v", err)
	}
	svc := bus.NewService(conflictRepo{Repository: repo})

	_, err := svc.CreateBook(ctx, "a1", "Emma", 1815)
	if !errors.Is(err, bus.ErrConflict) {
		t.Fatalf("CreateBook: got %v, want ErrConflict", err)
	}
}

func TestUpdateAuthorNotFound(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	_, err := svc.UpdateAuthor(ctx, "missing", "Name", "bio")
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("UpdateAuthor: got %v, want ErrNotFound", err)
	}
}

func TestDeleteAuthorNotFound(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))

	err := svc.DeleteAuthor(ctx, "missing")
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("DeleteAuthor: got %v, want ErrNotFound", err)
	}
}

func TestUpdateBookNotFound(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	_, err := svc.UpdateBook(ctx, authorID, "missing", "Emma", 1815)
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("UpdateBook: got %v, want ErrNotFound", err)
	}
}

func TestDeleteBookNotFound(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	err := svc.DeleteBook(ctx, authorID, "missing")
	if !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("DeleteBook: got %v, want ErrNotFound", err)
	}
}

func TestDeleteAuthorCascadesBooks(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	b1, err := svc.CreateBook(ctx, authorID, "Emma", 1815)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	b2, err := svc.CreateBook(ctx, authorID, "Pride and Prejudice", 1813)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	if err := svc.DeleteAuthor(ctx, authorID); err != nil {
		t.Fatalf("DeleteAuthor: %v", err)
	}

	// Both books must be soft-deleted (filtered on read).
	if _, err := svc.GetBook(ctx, authorID, b1.ID); !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("GetBook(%s) after cascade: got %v, want ErrNotFound", b1.ID, err)
	}
	if _, err := svc.GetBook(ctx, authorID, b2.ID); !errors.Is(err, bus.ErrNotFound) {
		t.Fatalf("GetBook(%s) after cascade: got %v, want ErrNotFound", b2.ID, err)
	}
}
func TestDeleteAuthorNoBooks(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")

	if err := svc.DeleteAuthor(ctx, authorID); err != nil {
		t.Fatalf("DeleteAuthor with no books: %v", err)
	}
}

func TestListAllBooks(t *testing.T) {
	ctx := context.Background()
	svc := bus.NewService(newRepo(t))
	authorID := seedAuthor(t, svc, "Jane Austen", "bio")
	otherID := seedAuthor(t, svc, "Charles Dickens", "bio")

	if _, err := svc.CreateBook(ctx, authorID, "Emma", 1815); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if _, err := svc.CreateBook(ctx, otherID, "Oliver Twist", 1838); err != nil {
		t.Fatalf("CreateBook: %v", err)
	}

	books, err := svc.ListAllBooks(ctx)
	if err != nil {
		t.Fatalf("ListAllBooks: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("ListAllBooks: got %d, want 2", len(books))
	}
}
