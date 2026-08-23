package main

import (
	"context"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/quells-bot/ddb-sqlite/examples/catalog/app"
	"github.com/quells-bot/ddb-sqlite/examples/catalog/bus"
	"github.com/quells-bot/ddb-sqlite/examples/catalog/storage"
	"github.com/quells-bot/ddb-sqlite/pkg/ddb"
	ddbsqlite "github.com/quells-bot/ddb-sqlite/pkg/ddb-sqlite"
)

type wires struct {
	App     app.Controller
	CleanUp []func() error
}

func wireUp(ctx context.Context) (w wires, err error) {
	var ddbAPI ddb.API
	if os.Getenv("DDB_MOCK") != "" {
		var ddbMock *ddbsqlite.Adapter
		ddbMock, err = ddbsqlite.Open(ctx, ":memory:")
		if err != nil {
			return
		}
		w.CleanUp = append(w.CleanUp, ddbMock.Close)
		ddbAPI = ddbMock
	} else {
		var cfg aws.Config
		cfg, err = config.LoadDefaultConfig(ctx)
		if err != nil {
			return
		}
		ddbAPI = dynamodb.NewFromConfig(cfg)
	}

	repo := storage.NewRepo(ddbAPI)
	if err = repo.EnsureTable(ctx); err != nil {
		return
	}
	svc := bus.NewService(repo)
	w.App = app.NewController(svc)
	return
}
