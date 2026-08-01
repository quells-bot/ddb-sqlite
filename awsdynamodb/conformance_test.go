package awsdynamodb_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
	"github.com/ory/dockertest/v4"

	"github.com/quells-bot/ddb-sqlite/awsdynamodb"
)

// api is the minimal interface (exact SDK method signatures) both *dynamodb.Client
// and *awsdynamodb.Adapter satisfy. The harness is parameterized by it so the
// same cases run against the adapter (pass 1) and dynamodb-local (pass 2).
type api interface {
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	ListTables(ctx context.Context, params *dynamodb.ListTablesInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error)
	DeleteTable(ctx context.Context, params *dynamodb.DeleteTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

type confTarget struct {
	name string
	ctor func(t *testing.T) (api, func())
}

var confTargets = []confTarget{
	{"adapter", newAdapterTarget},
	{"dynamodb-local", newLocalTarget},
}

func activeTargets() []confTarget {
	switch os.Getenv("DDBSQLITE_CONF_TARGET") {
	case "all":
		return confTargets
	case "dynamodb-local":
		return confTargets[1:]
	default:
		return confTargets[:1]
	}
}

func runConformance(t *testing.T, fn func(*testing.T, api)) {
	for _, tg := range activeTargets() {
		tg := tg
		t.Run(tg.name, func(t *testing.T) {
			c, cleanup := tg.ctor(t)
			defer cleanup()
			fn(t, c)
		})
	}
}

func newAdapterTarget(t *testing.T) (api, func()) {
	a, err := awsdynamodb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("adapter Open: %v", err)
	}
	return a, func() { _ = a.Close() }
}

// newLocalTarget returns the TestMain-managed dynamodb-local client. The
// container is started once in TestMain; here we hand out the shared client
// and a cleanup that purges all tables so each test starts clean. If the
// container could not be started (no socket/endpoint), every case skips
// with an actionable message.
func newLocalTarget(t *testing.T) (api, func()) {
	if localSkipReason != "" {
		t.Skip("dynamodb-local target unavailable: " + localSkipReason)
	}
	return localClient, func() {
		if err := purgeTables(context.Background(), localClient); err != nil {
			t.Errorf("purge tables after test: %v", err)
		}
	}
}

// --- dynamodb-local target (pass 2) ---

// dynamodb-local target state, owned by TestMain. The container is shared
// across the whole test binary; newLocalTarget hands out the shared client
// and returns a cleanup that purges all tables so each test starts clean.
var (
	localClient     api
	localPool       dockertest.ClosablePool
	localResource   dockertest.ClosableResource
	localSkipReason string
)

func localTargetActive() bool {
	switch os.Getenv("DDBSQLITE_CONF_TARGET") {
	case "all", "dynamodb-local":
		return true
	}
	return false
}

func TestMain(m *testing.M) {
	if localTargetActive() {
		if err := setupLocalTarget(context.Background()); err != nil {
			localSkipReason = err.Error()
		}
	}
	code := m.Run()
	teardownLocalTarget()
	os.Exit(code)
}

// setupLocalTarget either connects to a pre-existing endpoint
// (DDBSQLITE_CONF_LOCAL_ENDPOINT) or launches an ephemeral
// amazon/dynamodb-local:3.3.1 container via the Docker-compatible podman
// socket. On success localClient is set; on failure an error is returned,
// which becomes a per-test t.Skip via localSkipReason.
func setupLocalTarget(ctx context.Context) error {
	if ep := os.Getenv("DDBSQLITE_CONF_LOCAL_ENDPOINT"); ep != "" {
		c, err := newLocalClient(ctx, ep)
		if err != nil {
			return fmt.Errorf("DDBSQLITE_CONF_LOCAL_ENDPOINT: %w", err)
		}
		localClient = c
		return nil
	}
	if err := ensureDockerHost(); err != nil {
		return err
	}
	pool, err := dockertest.NewPool(ctx, "")
	if err != nil {
		return fmt.Errorf("connect to docker/podman: %w", err)
	}
	resource, err := pool.Run(ctx, "docker.io/amazon/dynamodb-local", dockertest.WithTag("3.3.1"))
	if err != nil {
		return fmt.Errorf("start dynamodb-local:3.3.1: %w", err)
	}
	localPool = pool
	localResource = resource
	endpoint := "http://localhost:" + resource.GetPort("8000/tcp")
	c, err := newLocalClient(ctx, endpoint)
	if err != nil {
		_ = resource.Close(ctx)
		localResource = nil
		return err
	}
	// Wait until dynamodb-local accepts a ListTables handshake.
	if err := pool.Retry(ctx, 60*time.Second, func() error {
		_, listErr := c.ListTables(ctx, &dynamodb.ListTablesInput{})
		return listErr
	}); err != nil {
		_ = resource.Close(ctx)
		localResource = nil
		return fmt.Errorf("dynamodb-local did not become ready: %w", err)
	}
	localClient = c
	return nil
}

func teardownLocalTarget() {
	if localResource != nil {
		_ = localResource.Close(context.Background())
		localResource = nil
	}
	if localPool != nil {
		_ = localPool.Close(context.Background())
		localPool = nil
	}
}

// ensureDockerHost points DOCKER_HOST at the rootless podman socket when it
// is unset. On this host `docker` is a shell alias for podman (rootless,
// uid 1000); there is no /var/run/docker.sock. Start the socket first:
//
//	systemctl --user start podman.socket
func ensureDockerHost() error {
	if os.Getenv("DOCKER_HOST") != "" {
		return nil
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		xdg = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	sock := xdg + "/podman/podman.sock"
	if _, err := os.Stat(sock); err != nil {
		return fmt.Errorf("no DOCKER_HOST and podman socket not found at %s (run: systemctl --user start podman.socket)", sock)
	}
	if err := os.Setenv("DOCKER_HOST", "unix://"+sock); err != nil {
		return fmt.Errorf("set DOCKER_HOST: %w", err)
	}
	return nil
}

// newLocalClient builds an SDK DynamoDB client pointed at endpoint with
// static dummy credentials (dynamodb-local does not validate them).
func newLocalClient(ctx context.Context, endpoint string) (api, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-west-2"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("local", "local", "")),
	)
	if err != nil {
		return nil, err
	}
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	}), nil
}

// purgeTables drops every table so the next test starts from a clean slate.
// Called from newLocalTarget's cleanup after each test that runs the local
// target against the shared container.
func purgeTables(ctx context.Context, c api) error {
	var start *string
	for {
		out, err := c.ListTables(ctx, &dynamodb.ListTablesInput{ExclusiveStartTableName: start, Limit: aws.Int32(100)})
		if err != nil {
			return err
		}
		for _, name := range out.TableNames {
			if _, err := c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(name)}); err != nil {
				var rnfe *types.ResourceNotFoundException
				if !errors.As(err, &rnfe) {
					return err
				}
			}
		}
		if out.LastEvaluatedTableName == nil {
			return nil
		}
		start = out.LastEvaluatedTableName
	}
}

// --- helpers ---

func mustCreate(t *testing.T, c api, ctx context.Context, name string) {
	t.Helper()
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(name),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
		BillingMode:          types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable %q: %v", name, err)
	}
}

func strVal(s string) types.AttributeValue { return &types.AttributeValueMemberS{Value: s} }

func asResourceNotFound(t *testing.T, err error, msg string) {
	t.Helper()
	var e *types.ResourceNotFoundException
	if !errors.As(err, &e) {
		t.Errorf("%s: err = %v, want ResourceNotFoundException", msg, err)
	}
}

func asValidation(t *testing.T, err error, msg string) {
	t.Helper()
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("%s: err = %v, want ValidationException", msg, err)
	}
}

// --- M1 conformance cases (all written against api) ---

func TestConfCreateTableDescribeTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Users")
		out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Users")})
		if err != nil {
			t.Fatalf("DescribeTable: %v", err)
		}
		if aws.ToString(out.Table.TableName) != "Users" {
			t.Errorf("TableName = %q", aws.ToString(out.Table.TableName))
		}
		// Describe a missing table -> ResourceNotFoundException.
		_, err = c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("nope")})
		asResourceNotFound(t, err, "describe missing")
	})
}

func TestConfListTables(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		for _, n := range []string{"alpha", "bravo", "charlie"} {
			mustCreate(t, c, ctx, n)
		}
		out, err := c.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(2)})
		if err != nil {
			t.Fatalf("ListTables: %v", err)
		}
		if len(out.TableNames) != 2 || out.TableNames[0] != "alpha" {
			t.Errorf("page 1 = %v", out.TableNames)
		}
		if aws.ToString(out.LastEvaluatedTableName) != "bravo" {
			t.Errorf("LastEvaluated = %q", aws.ToString(out.LastEvaluatedTableName))
		}
		out2, _ := c.ListTables(ctx, &dynamodb.ListTablesInput{ExclusiveStartTableName: out.LastEvaluatedTableName, Limit: aws.Int32(2)})
		if len(out2.TableNames) != 1 || out2.TableNames[0] != "charlie" {
			t.Errorf("page 2 = %v", out2.TableNames)
		}
		if out2.LastEvaluatedTableName != nil {
			t.Errorf("LastEvaluated = %v, want nil", out2.LastEvaluatedTableName)
		}
	})
}

func TestConfDeleteTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Tbl")
		if _, err := c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String("Tbl")}); err != nil {
			t.Fatalf("DeleteTable: %v", err)
		}
		_, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Tbl")})
		asResourceNotFound(t, err, "describe after delete")
		// Delete a missing table -> ResourceNotFoundException.
		_, err = c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String("Tbl")})
		asResourceNotFound(t, err, "delete missing")
	})
}

func TestConfPutGetItemAllTypes(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Tbl")
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("Tbl"),
			Item: map[string]types.AttributeValue{
				"pk": strVal("k"),
				"s":  strVal("hi"),
				"n":  &types.AttributeValueMemberN{Value: "12.5"},
				"b":  &types.AttributeValueMemberB{Value: []byte{0, 255}},
				"bl": &types.AttributeValueMemberBOOL{Value: true},
				"nl": &types.AttributeValueMemberNULL{Value: true},
				"l":  &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("a")}},
				"m":  &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"i": strVal("v")}},
				"ss": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
				"ns": &types.AttributeValueMemberNS{Value: []string{"1", "2"}},
				"bs": &types.AttributeValueMemberBS{Value: [][]byte{{1}, {2}}},
			},
		})
		if err != nil {
			t.Fatalf("PutItem: %v", err)
		}
		got, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("Tbl"), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got.Item == nil {
			t.Fatal("nil Item")
		}
		if got.Item["n"].(*types.AttributeValueMemberN).Value != "12.5" {
			t.Errorf("n = %q", got.Item["n"].(*types.AttributeValueMemberN).Value)
		}
		if got.Item["ns"].(*types.AttributeValueMemberNS).Value[0] != "1" {
			t.Errorf("ns = %v", got.Item["ns"].(*types.AttributeValueMemberNS).Value)
		}
	})
}

func TestConfPutItemOverwrite(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Tbl")
		c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Tbl"), Item: map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("first")}})
		c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Tbl"), Item: map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("second")}})
		got, _ := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("Tbl"), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
		if got.Item["v"].(*types.AttributeValueMemberS).Value != "second" {
			t.Errorf("v = %q, want second", got.Item["v"].(*types.AttributeValueMemberS).Value)
		}
	})
}

func TestConfDeleteItem(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Tbl")
		c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Tbl"), Item: map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("x")}})
		if _, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String("Tbl"), Key: map[string]types.AttributeValue{"pk": strVal("k")}}); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		got, _ := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("Tbl"), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
		if got.Item != nil {
			t.Errorf("after delete, Item = %v, want nil", got.Item)
		}
		// Idempotent delete of missing key: no error.
		if _, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String("Tbl"), Key: map[string]types.AttributeValue{"pk": strVal("k")}}); err != nil {
			t.Errorf("delete missing: %v", err)
		}
	})
}

func TestConfPutItemSizeLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Tbl")
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Tbl"), Item: map[string]types.AttributeValue{
			"pk":   strVal("k"),
			"data": strVal(strings.Repeat("x", 400*1024+1)),
		}})
		asValidation(t, err, "oversized item")
	})
}

func TestConfPutItemKeyValidation(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		// Table with N partition key.
		c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName:            aws.String("Num"),
			KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeN}},
			BillingMode:          types.BillingModePayPerRequest,
		})
		// Missing partition key.
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Num"), Item: map[string]types.AttributeValue{"other": strVal("x")}})
		asValidation(t, err, "missing pk")
		// Type mismatch: declared N, supplied S.
		_, err = c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Num"), Item: map[string]types.AttributeValue{"pk": strVal("x")}})
		asValidation(t, err, "pk type mismatch")
		// Malformed NumberSet member.
		_, err = c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Num"), Item: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberN{Value: "1"},
			"ns": &types.AttributeValueMemberNS{Value: []string{"1", "notanumber"}},
		}})
		asValidation(t, err, "malformed NS member")
	})
}

func TestConfUnknownTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("nope"), Item: map[string]types.AttributeValue{"pk": strVal("k")}})
		asResourceNotFound(t, err, "PutItem unknown table")
		_, err = c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("nope"), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
		asResourceNotFound(t, err, "GetItem unknown table")
		_, err = c.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String("nope"), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
		asResourceNotFound(t, err, "DeleteItem unknown table")
	})
}
