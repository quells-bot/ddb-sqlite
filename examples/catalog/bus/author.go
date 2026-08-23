package bus

import (
	"context"
	"errors"
	"fmt"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/storage"
)

type Author struct {
	ID   string
	Name string
	Bio  string
}

func (s service) GetAuthor(ctx context.Context, id string) (*Author, error) {
	a, err := s.repo.GetAuthor(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &Author{ID: a.ID, Name: a.Name, Bio: a.Bio}, nil
}

func (s service) CreateAuthor(ctx context.Context, name, bio string) (*Author, error) {
	if name == "" {
		return nil, fmt.Errorf("name must not be empty: %w", ErrValidation)
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	a := &storage.Author{ID: id, Name: name, Bio: bio}
	if err := s.repo.PutAuthor(ctx, a); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return &Author{ID: id, Name: name, Bio: bio}, nil
}

func (s service) UpdateAuthor(ctx context.Context, id, name, bio string) (*Author, error) {
	if name == "" {
		return nil, fmt.Errorf("name must not be empty: %w", ErrValidation)
	}
	if err := s.repo.UpdateAuthor(ctx, id, name, bio); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.GetAuthor(ctx, id)
}

func (s service) DeleteAuthor(ctx context.Context, id string) error {
	if err := s.repo.DeleteAuthor(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	// Cascade: soft-delete all live books under this author.
	books, err := s.repo.ListBooks(ctx, id)
	if err != nil {
		return err
	}
	if len(books) > 0 {
		if err := s.repo.SoftDeleteBooks(ctx, books); err != nil {
			return err
		}
	}
	return nil
}

func (s service) ListAuthors(ctx context.Context) ([]*Author, error) {
	authors, err := s.repo.ListAuthors(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Author, 0, len(authors))
	for _, a := range authors {
		out = append(out, &Author{ID: a.ID, Name: a.Name, Bio: a.Bio})
	}
	return out, nil
}
