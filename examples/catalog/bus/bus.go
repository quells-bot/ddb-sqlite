package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/storage"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrValidation = errors.New("validation failed")
	ErrConflict   = errors.New("conflict")
)

type Service interface {
	GetAuthor(ctx context.Context, id string) (*Author, error)
	CreateAuthor(ctx context.Context, name, bio string) (*Author, error)
	UpdateAuthor(ctx context.Context, id, name, bio string) (*Author, error)
	DeleteAuthor(ctx context.Context, id string) error
	ListAuthors(ctx context.Context) ([]*Author, error)
	GetBook(ctx context.Context, authorID, bookID string) (*Book, error)
	CreateBook(ctx context.Context, authorID, title string, year int) (*Book, error)
	UpdateBook(ctx context.Context, authorID, bookID, title string, year int) (*Book, error)
	DeleteBook(ctx context.Context, authorID, bookID string) error
	ListBooks(ctx context.Context, authorID string) ([]*Book, error)
	ListAllBooks(ctx context.Context) ([]*Book, error)
}

var _ Service = (*service)(nil)

type service struct {
	repo storage.Repository
}

func NewService(repo storage.Repository) *service {
	return &service{
		repo: repo,
	}
}

// newID returns a 32-character hex string from 16 random bytes. Unique enough
// for an example; not an RFC 4122 UUID.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
