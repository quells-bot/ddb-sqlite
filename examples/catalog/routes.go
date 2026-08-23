package main

import (
	"net/http"

	"github.com/quells-bot/ddb-sqlite/examples/catalog/app"
)

func routes(mux *http.ServeMux, app app.Controller) {
	mux.Handle("/authors", methods{
		http.MethodGet:  app.ListAuthors,
		http.MethodPost: app.CreateAuthor,
	})
	mux.Handle("/authors/{authorID}", methods{
		http.MethodGet:    app.GetAuthor,
		http.MethodPut:    app.UpdateAuthor,
		http.MethodDelete: app.DeleteAuthor,
	})
	mux.Handle("/authors/{authorID}/books", methods{
		http.MethodGet:  app.ListBooks,
		http.MethodPost: app.CreateBook,
	})
	mux.Handle("/authors/{authorID}/books/{bookID}", methods{
		http.MethodGet:    app.GetBook,
		http.MethodPut:    app.UpdateBook,
		http.MethodDelete: app.DeleteBook,
	})
	mux.Handle("/books", methods{
		http.MethodGet: app.ListAllBooks,
	})
}

type methods map[string]http.HandlerFunc

func (m methods) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	handler, ok := m[req.Method]
	if !ok {
		http.Error(rw, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	handler.ServeHTTP(rw, req)
}
