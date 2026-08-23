package bus

import (
	"context"
	"errors"
	"fmt"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/storage"
)

type Book struct {
	ID       string
	AuthorID string
	Title    string
	Year     int
}

func (s service) GetBook(ctx context.Context, authorID, bookID string) (*Book, error) {
	b, err := s.repo.GetBook(ctx, authorID, bookID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &Book{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year}, nil
}

func (s service) CreateBook(ctx context.Context, authorID, title string, year int) (*Book, error) {
	if title == "" {
		return nil, fmt.Errorf("title must not be empty: %w", ErrValidation)
	}
	if year <= 0 {
		return nil, fmt.Errorf("year must be positive: %w", ErrValidation)
	}
	if _, err := s.repo.GetAuthor(ctx, authorID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	b := &storage.Book{ID: id, AuthorID: authorID, Title: title, Year: year}
	if err := s.repo.PutBook(ctx, b); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return &Book{ID: id, AuthorID: authorID, Title: title, Year: year}, nil
}

func (s service) UpdateBook(ctx context.Context, authorID, bookID, title string, year int) (*Book, error) {
	if title == "" {
		return nil, fmt.Errorf("title must not be empty: %w", ErrValidation)
	}
	if year <= 0 {
		return nil, fmt.Errorf("year must be positive: %w", ErrValidation)
	}
	if err := s.repo.UpdateBook(ctx, authorID, bookID, title, year); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.GetBook(ctx, authorID, bookID)
}

func (s service) DeleteBook(ctx context.Context, authorID, bookID string) error {
	if err := s.repo.DeleteBook(ctx, authorID, bookID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s service) ListBooks(ctx context.Context, authorID string) ([]*Book, error) {
	if _, err := s.repo.GetAuthor(ctx, authorID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	books, err := s.repo.ListBooks(ctx, authorID)
	if err != nil {
		return nil, err
	}
	out := make([]*Book, 0, len(books))
	for _, b := range books {
		out = append(out, &Book{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year})
	}
	return out, nil
}

func (s service) ListAllBooks(ctx context.Context) ([]*Book, error) {
	books, err := s.repo.ListAllBooks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Book, 0, len(books))
	for _, b := range books {
		out = append(out, &Book{ID: b.ID, AuthorID: b.AuthorID, Title: b.Title, Year: b.Year})
	}
	return out, nil
}
