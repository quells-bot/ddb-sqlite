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
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
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
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
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

// --- M2 pass 1 conformance helpers ---

func numVal(s string) types.AttributeValue { return &types.AttributeValueMemberN{Value: s} }

// mustExpr builds a DynamoDB expression from the builder, failing the test on
// error. The returned Expression's Condition/Update/Filter getters and
// Names()/Values() maps are spread into SDK input structs. Using the builder
// keeps placeholder binding correct without hand-managed #name/:value maps.
func mustExpr(t *testing.T, b expression.Builder) expression.Expression {
	t.Helper()
	expr, err := b.Build()
	if err != nil {
		t.Fatalf("build expression: %v", err)
	}
	return expr
}

func asConditionalCheckFailed(t *testing.T, err error, msg string) *types.ConditionalCheckFailedException {
	t.Helper()
	var e *types.ConditionalCheckFailedException
	if !errors.As(err, &e) {
		t.Errorf("%s: err = %v, want ConditionalCheckFailedException", msg, err)
		return nil
	}
	return e
}

// putConf seeds one item, failing the test on error.
func putConf(t *testing.T, c api, ctx context.Context, table string, item map[string]types.AttributeValue) {
	t.Helper()
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: item}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
}

// --- M2 pass 1 conformance cases ---

// Case 1: conditional put.
func TestConfConditionalPut(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Cond")

		// attribute_not_exists succeeds on insert, fails on overwrite.
		notExists := mustExpr(t, expression.NewBuilder().
			WithCondition(expression.AttributeNotExists(expression.Name("pk"))))

		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Cond"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("first")},
			ConditionExpression:       notExists.Condition(),
			ExpressionAttributeNames:  notExists.Names(),
			ExpressionAttributeValues: notExists.Values(),
		}); err != nil {
			t.Fatalf("conditional insert: %v", err)
		}

		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Cond"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("second")},
			ConditionExpression:       notExists.Condition(),
			ExpressionAttributeNames:  notExists.Names(),
			ExpressionAttributeValues: notExists.Values(),
		})
		asConditionalCheckFailed(t, err, "overwrite with attribute_not_exists")

		// The failed write left the item untouched.
		out, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String("Cond"),
			Key:       map[string]types.AttributeValue{"pk": strVal("k")},
		})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got := out.Item["v"].(*types.AttributeValueMemberS).Value; got != "first" {
			t.Errorf("v = %q, want first", got)
		}

		// A satisfied value condition succeeds.
		equalsFirst := mustExpr(t, expression.NewBuilder().
			WithCondition(expression.Name("v").Equal(expression.Value(strVal("first")))))

		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Cond"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("second")},
			ConditionExpression:       equalsFirst.Condition(),
			ExpressionAttributeNames:  equalsFirst.Names(),
			ExpressionAttributeValues: equalsFirst.Values(),
		}); err != nil {
			t.Fatalf("conditional overwrite: %v", err)
		}
	})
}

// Case 2: conditional delete.
func TestConfConditionalDelete(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "CondDel")
		putConf(t, c, ctx, "CondDel", map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("first")})

		// Unsatisfied condition -> the item survives.
		unsat := mustExpr(t, expression.NewBuilder().
			WithCondition(expression.Name("v").Equal(expression.Value(strVal("other")))))
		_, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName:                 aws.String("CondDel"),
			Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       unsat.Condition(),
			ExpressionAttributeNames:  unsat.Names(),
			ExpressionAttributeValues: unsat.Values(),
		})
		asConditionalCheckFailed(t, err, "delete with unsatisfied condition")

		out, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String("CondDel"),
			Key:       map[string]types.AttributeValue{"pk": strVal("k")},
		})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if len(out.Item) == 0 {
			t.Fatal("item was deleted despite a failed condition")
		}

		// Satisfied condition -> the item goes.
		sat := mustExpr(t, expression.NewBuilder().
			WithCondition(expression.Name("v").Equal(expression.Value(strVal("first")))))
		if _, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName:                 aws.String("CondDel"),
			Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       sat.Condition(),
			ExpressionAttributeNames:  sat.Names(),
			ExpressionAttributeValues: sat.Values(),
		}); err != nil {
			t.Fatalf("conditional delete: %v", err)
		}
		out, err = c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String("CondDel"),
			Key:       map[string]types.AttributeValue{"pk": strVal("k")},
		})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if len(out.Item) != 0 {
			t.Errorf("item = %v, want deleted", out.Item)
		}
	})
}

// Case 3: ReturnValues=ALL_OLD on Put and Delete.
func TestConfReturnValuesAllOld(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "RetVals")

		// First write: nothing to return.
		out, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:    aws.String("RetVals"),
			Item:         map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("first")},
			ReturnValues: types.ReturnValueAllOld,
		})
		if err != nil {
			t.Fatalf("PutItem: %v", err)
		}
		if len(out.Attributes) != 0 {
			t.Errorf("Attributes = %v, want empty on first write", out.Attributes)
		}

		// Overwrite returns the prior item.
		out, err = c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:    aws.String("RetVals"),
			Item:         map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("second")},
			ReturnValues: types.ReturnValueAllOld,
		})
		if err != nil {
			t.Fatalf("PutItem overwrite: %v", err)
		}
		if got := out.Attributes["v"].(*types.AttributeValueMemberS).Value; got != "first" {
			t.Errorf("Attributes[v] = %q, want first", got)
		}

		// Delete returns the deleted item; a second delete returns nothing.
		dout, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName:    aws.String("RetVals"),
			Key:          map[string]types.AttributeValue{"pk": strVal("k")},
			ReturnValues: types.ReturnValueAllOld,
		})
		if err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		if got := dout.Attributes["v"].(*types.AttributeValueMemberS).Value; got != "second" {
			t.Errorf("Attributes[v] = %q, want second", got)
		}
		dout, err = c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName:    aws.String("RetVals"),
			Key:          map[string]types.AttributeValue{"pk": strVal("k")},
			ReturnValues: types.ReturnValueAllOld,
		})
		if err != nil {
			t.Fatalf("DeleteItem idempotent: %v", err)
		}
		if len(dout.Attributes) != 0 {
			t.Errorf("Attributes = %v, want empty", dout.Attributes)
		}
	})
}

// seedSemItem is the case-4 fixture, exercising all ten attribute types. Cases
// that satisfy their condition overwrite the item, so it is re-seeded between
// subtests.
func seedSemItem() map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk":   strVal("k"),
		"s":    strVal("hello"),
		"n":    numVal("42"),
		"b":    &types.AttributeValueMemberB{Value: []byte{1, 2, 3}},
		"bool": &types.AttributeValueMemberBOOL{Value: true},
		"null": &types.AttributeValueMemberNULL{Value: true},
		"l":    &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("x"), numVal("7")}},
		"m":    &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"inner": strVal("deep")}},
		"ss":   &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
		"ns":   &types.AttributeValueMemberNS{Value: []string{"1", "2"}},
		"bs":   &types.AttributeValueMemberBS{Value: [][]byte{{9}}},
	}
}

// Case 4: comparator and function semantics over all ten types, missing vs
// NULL, and type-mismatch-is-false.
//
// A boolean expression result is observed through the SDK by re-putting the
// SAME key under the condition: a satisfied condition succeeds, an unsatisfied
// one raises ConditionalCheckFailedException.
func TestConfConditionSemantics(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Sem")
		putConf(t, c, ctx, "Sem", seedSemItem())

		cases := []struct {
			name string
			cond expression.ConditionBuilder
			want bool
		}{
			{"string equality", expression.Name("s").Equal(expression.Value(strVal("hello"))), true},
			{"number equality ignores trailing zero", expression.Name("n").Equal(expression.Value(numVal("42.0"))), true},
			{"binary equality", expression.Name("b").Equal(expression.Value(&types.AttributeValueMemberB{Value: []byte{1, 2, 3}})), true},
			{"bool equality", expression.Name("bool").Equal(expression.Value(&types.AttributeValueMemberBOOL{Value: true})), true},
			// "null" is a DynamoDB reserved word; the builder aliases every name.
			{"null equality", expression.Name("null").Equal(expression.Value(&types.AttributeValueMemberNULL{Value: true})), true},
			{"number ordering", expression.Name("n").LessThan(expression.Value(numVal("100"))), true},
			{"string ordering", expression.Name("s").LessThan(expression.Value(strVal("world"))), true},

			{"cross-type equality is false", expression.Name("s").Equal(expression.Value(numVal("42"))), false},
			{"cross-type inequality is true", expression.Name("s").NotEqual(expression.Value(numVal("42"))), true},
			{"cross-type ordering is false", expression.Name("s").LessThan(expression.Value(numVal("42"))), false},

			{"missing equality is false", expression.Name("nope").Equal(expression.Value(strVal("hello"))), false},
			// The reference evaluates <> with a missing operand to TRUE: a missing
			// attribute is by definition not equal to anything (spec §4.2).
			{"missing inequality is true", expression.Name("nope").NotEqual(expression.Value(strVal("hello"))), true},

			{"attribute_exists on a present NULL", expression.AttributeExists(expression.Name("null")), true},
			{"attribute_not_exists on a present NULL", expression.AttributeNotExists(expression.Name("null")), false},
			{"attribute_not_exists on a missing attribute", expression.AttributeNotExists(expression.Name("nope")), true},
			{"attribute_exists nested", expression.AttributeExists(expression.Name("m.inner")), true},

			{"attribute_type match", expression.Name("s").AttributeType(expression.String), true},
			{"attribute_type mismatch", expression.Name("s").AttributeType(expression.Number), false},
			{"attribute_type NULL", expression.Name("null").AttributeType(expression.Null), true},

			{"contains substring", expression.Contains(expression.Name("s"), strVal("ell")), true},
			{"contains string set member", expression.Contains(expression.Name("ss"), strVal("a")), true},
			{"contains number set member", expression.Contains(expression.Name("ns"), numVal("1")), true},
			{"contains binary set member", expression.Contains(expression.Name("bs"), &types.AttributeValueMemberB{Value: []byte{9}}), true},
			{"contains list element", expression.Contains(expression.Name("l"), strVal("x")), true},
			{"contains type mismatch is false", expression.Contains(expression.Name("s"), numVal("1")), false},

			{"begins_with string", expression.BeginsWith(expression.Name("s"), "he"), true},
			{"begins_with absent prefix", expression.BeginsWith(expression.Name("s"), "zz"), false},

			{"in matches", expression.In(expression.Name("s"), expression.Value(strVal("world")), expression.Value(strVal("hello"))), true},
			{"in does not match", expression.In(expression.Name("s"), expression.Value(strVal("world")), expression.Value(strVal("zz"))), false},

			{"between inside range", expression.Between(expression.Name("n"), expression.Value(numVal("10")), expression.Value(numVal("100"))), true},
			{"between outside range", expression.Between(expression.Name("n"), expression.Value(numVal("100")), expression.Value(numVal("200"))), false},

			{"and", expression.And(expression.Name("s").Equal(expression.Value(strVal("hello"))), expression.AttributeExists(expression.Name("n"))), true},
			{"or", expression.Or(expression.Name("s").Equal(expression.Value(strVal("zz"))), expression.AttributeExists(expression.Name("n"))), true},
			{"not", expression.Not(expression.Name("s").Equal(expression.Value(strVal("zz")))), true},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				expr := mustExpr(t, expression.NewBuilder().WithCondition(tc.cond))
				_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
					TableName:                 aws.String("Sem"),
					Item:                      map[string]types.AttributeValue{"pk": strVal("k"), "marker": strVal(tc.name)},
					ConditionExpression:       expr.Condition(),
					ExpressionAttributeNames:  expr.Names(),
					ExpressionAttributeValues: expr.Values(),
				})
				if tc.want {
					if err != nil {
						t.Fatalf("condition %s should have been satisfied: %v", tc.name, err)
					}
					// The successful write replaced the fixture; restore it.
					putConf(t, c, ctx, "Sem", seedSemItem())
					return
				}
				asConditionalCheckFailed(t, err, "condition "+tc.name)
			})
		}

		t.Run("contains on a mixed-type list", func(t *testing.T) {
			// l is [S "x", N 7]. Searching for the N element requires scanning
			// past a type-mismatched element. dynamodb-local reports no error
			// (the condition matches), so the scan SKIPS mismatched elements;
			// evalContains' TagList branch is correct (spec §4.3(2)).
			expr := mustExpr(t, expression.NewBuilder().
				WithCondition(expression.Contains(expression.Name("l"), numVal("7"))))
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String("Sem"),
				Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
				ConditionExpression:       expr.Condition(),
				ExpressionAttributeNames:  expr.Names(),
				ExpressionAttributeValues: expr.Values(),
			})
			if err != nil {
				t.Fatalf("contains(l, 7) on [S, N] should have matched: %v", err)
			}
			putConf(t, c, ctx, "Sem", seedSemItem())
		})
	})
}

// Case 5: BETWEEN with reversed bounds is a validation error, not false.
func TestConfBetweenReversedBounds(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Btw")
		putConf(t, c, ctx, "Btw", map[string]types.AttributeValue{"pk": strVal("k"), "n": numVal("42")})

		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Btw"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String("n BETWEEN :hi AND :lo"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":lo": numVal("10"), ":hi": numVal("100")},
		})
		asValidation(t, err, "BETWEEN with reversed bounds")
	})
}

// Case 6: size() over the supported types, plus the N probe that settles
// spec §4.3(1).
func TestConfSize(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Size")
		putConf(t, c, ctx, "Size", map[string]types.AttributeValue{
			"pk": strVal("k"),
			"s":  strVal("hello"),
			"b":  &types.AttributeValueMemberB{Value: []byte{1, 2, 3}},
			"l":  &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("x"), strVal("y")}},
			"m":  &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"a": strVal("1"), "b": strVal("2")}},
			"ss": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
		})

		seed := map[string]types.AttributeValue{
			"pk": strVal("k"), "s": strVal("hello"),
			"b":  &types.AttributeValueMemberB{Value: []byte{1, 2, 3}},
			"l":  &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("x"), strVal("y")}},
			"m":  &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"a": strVal("1"), "b": strVal("2")}},
			"ss": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
		}

		cases := []struct {
			name string
			attr string
			n    string
		}{
			{"string is byte length", "s", "5"},
			{"binary is byte count", "b", "3"},
			{"list is element count", "l", "2"},
			{"map is entry count", "m", "2"},
			{"string set is element count", "ss", "2"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				expr := mustExpr(t, expression.NewBuilder().
					WithCondition(expression.Size(expression.Name(tc.attr)).Equal(expression.Value(numVal(tc.n)))))
				if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
					TableName:                 aws.String("Size"),
					Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
					ConditionExpression:       expr.Condition(),
					ExpressionAttributeNames:  expr.Names(),
					ExpressionAttributeValues: expr.Values(),
				}); err != nil {
					t.Errorf("size(%s) = %s: %v", tc.attr, tc.n, err)
				}
				putConf(t, c, ctx, "Size", seed)
			})
		}

		// size() on a missing attribute makes the comparison false.
		missingSize := mustExpr(t, expression.NewBuilder().
			WithCondition(expression.Size(expression.Name("nope")).Equal(expression.Value(numVal("1")))))
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Size"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       missingSize.Condition(),
			ExpressionAttributeNames:  missingSize.Names(),
			ExpressionAttributeValues: missingSize.Values(),
		})
		asConditionalCheckFailed(t, err, "size() of a missing attribute")

		// Probe: spec §4.3(1). Real DynamoDB's documentation does not list N
		// among size()'s supported types; the parent spec §5.1 claims a digit
		// count. dynamodb-local reports ConditionalCheckFailed for both the
		// correct digit count and a wrong one, so size(N) is undefined and the
		// comparison is simply false; parent spec §5.1 was amended accordingly.
		t.Run("size of a number", func(t *testing.T) {
			putConf(t, c, ctx, "Size", map[string]types.AttributeValue{"pk": strVal("k"), "n": numVal("12345")})
			numSize := mustExpr(t, expression.NewBuilder().
				WithCondition(expression.Size(expression.Name("n")).Equal(expression.Value(numVal("5")))))
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String("Size"),
				Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
				ConditionExpression:       numSize.Condition(),
				ExpressionAttributeNames:  numSize.Names(),
				ExpressionAttributeValues: numSize.Values(),
			})
			asConditionalCheckFailed(t, err, "size() of a Number")
		})
	})
}

// Case 7: substitution validation — undefined and unused entries.
func TestConfSubstitutionValidation(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Subst")
		putConf(t, c, ctx, "Subst", map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("x")})

		item := map[string]types.AttributeValue{"pk": strVal("k")}

		t.Run("undefined name", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String("Subst"),
				Item:                      item,
				ConditionExpression:       aws.String("#missing = :v"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
			})
			asValidation(t, err, "undefined #name")
		})

		t.Run("undefined value", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:           aws.String("Subst"),
				Item:                item,
				ConditionExpression: aws.String("v = :missing"),
			})
			asValidation(t, err, "undefined :value")
		})

		t.Run("unused name", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                aws.String("Subst"),
				Item:                     item,
				ConditionExpression:      aws.String("attribute_exists(v)"),
				ExpressionAttributeNames: map[string]string{"#spare": "v"},
			})
			asValidation(t, err, "unused #name")
		})

		t.Run("unused value", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String("Subst"),
				Item:                      item,
				ConditionExpression:       aws.String("attribute_exists(v)"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":spare": strVal("x")},
			})
			asValidation(t, err, "unused :value")
		})

		t.Run("maps supplied with no expression", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String("Subst"),
				Item:                      item,
				ExpressionAttributeValues: map[string]types.AttributeValue{":spare": strVal("x")},
			})
			asValidation(t, err, "values with no expression")
		})

		t.Run("malformed expression", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:           aws.String("Subst"),
				Item:                item,
				ConditionExpression: aws.String("attribute_exists("),
			})
			asValidation(t, err, "malformed expression")
		})

		t.Run("substitutions split across two expressions", func(t *testing.T) {
			// #n is referenced only by the ConditionExpression and :v only by the
			// UpdateExpression. Neither is unused: DynamoDB validates the maps
			// against the UNION of every expression's refs (spec §4.5).
			if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("Subst"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				ConditionExpression:       aws.String("attribute_exists(#n)"),
				UpdateExpression:          aws.String("SET marker = :v"),
				ExpressionAttributeNames:  map[string]string{"#n": "v"},
				ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("set")},
			}); err != nil {
				t.Fatalf("substitutions split across expressions: %v", err)
			}
		})
	})
}

// --- M2 pass 2 conformance cases ---

// getConf reads one item by string partition key.
func getConf(t *testing.T, c api, ctx context.Context, table, pk string) map[string]types.AttributeValue {
	t.Helper()
	out, err := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key:       map[string]types.AttributeValue{"pk": strVal(pk)},
	})
	if err != nil {
		t.Fatalf("GetItem(%q): %v", pk, err)
	}
	return out.Item
}

// Case 8: UpdateItem upsert on an absent key.
func TestConfUpdateUpsert(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Upsert")

		// An update against an absent key creates the item.
		setS := mustExpr(t, expression.NewBuilder().
			WithUpdate(expression.Set(expression.Name("s"), expression.Value(strVal("v")))))
		if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String("Upsert"),
			Key:                       map[string]types.AttributeValue{"pk": strVal("created")},
			UpdateExpression:          setS.Update(),
			ExpressionAttributeNames:  setS.Names(),
			ExpressionAttributeValues: setS.Values(),
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got := getConf(t, c, ctx, "Upsert", "created")
		if got["s"].(*types.AttributeValueMemberS).Value != "v" {
			t.Errorf("item = %v, want s=v", got)
		}
		if _, ok := got["pk"]; !ok {
			t.Error("the created item is missing its key attribute")
		}

		// No UpdateExpression at all creates a key-only item.
		if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String("Upsert"),
			Key:       map[string]types.AttributeValue{"pk": strVal("bare")},
		}); err != nil {
			t.Fatalf("key-only upsert: %v", err)
		}
		if got := getConf(t, c, ctx, "Upsert", "bare"); len(got) != 1 {
			t.Errorf("item = %v, want the key alone", got)
		}

		// ADD against an absent item yields the addend.
		addN := mustExpr(t, expression.NewBuilder().
			WithUpdate(expression.Add(expression.Name("n"), expression.Value(numVal("1")))))
		if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String("Upsert"),
			Key:                       map[string]types.AttributeValue{"pk": strVal("hits")},
			UpdateExpression:          addN.Update(),
			ExpressionAttributeNames:  addN.Names(),
			ExpressionAttributeValues: addN.Values(),
		}); err != nil {
			t.Fatalf("ADD upsert: %v", err)
		}
		if got := getConf(t, c, ctx, "Upsert", "hits"); got["n"].(*types.AttributeValueMemberN).Value != "1" {
			t.Errorf("n = %v, want 1", got["n"])
		}
	})
}

// Case 9: every update action.
func TestConfUpdateActions(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Upd")

		seed := func() {
			t.Helper()
			putConf(t, c, ctx, "Upd", map[string]types.AttributeValue{
				"pk": strVal("k"),
				"s":  strVal("hello"),
				"n":  numVal("42"),
				"l":  &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("x"), strVal("y")}},
				"m":  &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"deep": strVal("v")}},
				"ss": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
			})
		}

		cases := []struct {
			name  string
			upd   expression.UpdateBuilder
			check func(t *testing.T, got map[string]types.AttributeValue)
		}{
			{
				name: "SET a scalar",
				upd:  expression.Set(expression.Name("s"), expression.Value(strVal("bye"))),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if got["s"].(*types.AttributeValueMemberS).Value != "bye" {
						t.Errorf("s = %v", got["s"])
					}
				},
			},
			{
				name: "SET a nested path",
				upd:  expression.Set(expression.Name("m.extra"), expression.Value(strVal("nested"))),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					m := got["m"].(*types.AttributeValueMemberM).Value
					if m["extra"].(*types.AttributeValueMemberS).Value != "nested" {
						t.Errorf("m = %v", m)
					}
					if _, ok := m["deep"]; !ok {
						t.Error("the sibling map entry was lost")
					}
				},
			},
			{
				name: "SET arithmetic",
				upd:  expression.Set(expression.Name("n"), expression.Name("n").Plus(expression.Value(numVal("1")))),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if got["n"].(*types.AttributeValueMemberN).Value != "43" {
						t.Errorf("n = %v, want 43", got["n"])
					}
				},
			},
			{
				name: "SET if_not_exists keeps the existing value",
				upd:  expression.Set(expression.Name("s"), expression.IfNotExists(expression.Name("s"), expression.Value(strVal("fallback")))),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if got["s"].(*types.AttributeValueMemberS).Value != "hello" {
						t.Errorf("s = %v, want the existing hello", got["s"])
					}
				},
			},
			{
				name: "SET if_not_exists supplies the fallback",
				upd:  expression.Set(expression.Name("fresh"), expression.IfNotExists(expression.Name("fresh"), expression.Value(strVal("fallback")))),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if got["fresh"].(*types.AttributeValueMemberS).Value != "fallback" {
						t.Errorf("fresh = %v", got["fresh"])
					}
				},
			},
			{
				name: "SET list_append",
				upd: expression.Set(expression.Name("l"), expression.Name("l").ListAppend(
					expression.Value(&types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("z")}}),
				)),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					l := got["l"].(*types.AttributeValueMemberL).Value
					if len(l) != 3 || l[2].(*types.AttributeValueMemberS).Value != "z" {
						t.Errorf("l = %v", l)
					}
				},
			},
			{
				name: "REMOVE an attribute",
				upd:  expression.Remove(expression.Name("s")),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if _, ok := got["s"]; ok {
						t.Error("s survived REMOVE")
					}
				},
			},
			{
				name: "REMOVE a list index shifts the rest down",
				upd:  expression.Remove(expression.Name("l[0]")),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					l := got["l"].(*types.AttributeValueMemberL).Value
					if len(l) != 1 || l[0].(*types.AttributeValueMemberS).Value != "y" {
						t.Errorf("l = %v, want [y]", l)
					}
				},
			},
			{
				name: "ADD a number",
				upd:  expression.Add(expression.Name("n"), expression.Value(numVal("1"))),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if got["n"].(*types.AttributeValueMemberN).Value != "43" {
						t.Errorf("n = %v, want 43", got["n"])
					}
				},
			},
			{
				name: "ADD unions a set",
				upd:  expression.Add(expression.Name("ss"), expression.Value(&types.AttributeValueMemberSS{Value: []string{"b", "c"}})),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if n := len(got["ss"].(*types.AttributeValueMemberSS).Value); n != 3 {
						t.Errorf("ss = %v, want three deduped members", got["ss"])
					}
				},
			},
			{
				name: "DELETE removes set members",
				upd:  expression.Delete(expression.Name("ss"), expression.Value(&types.AttributeValueMemberSS{Value: []string{"a"}})),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					ss := got["ss"].(*types.AttributeValueMemberSS).Value
					if len(ss) != 1 || ss[0] != "b" {
						t.Errorf("ss = %v, want [b]", ss)
					}
				},
			},
			{
				name: "DELETE emptying a set removes the attribute",
				upd:  expression.Delete(expression.Name("ss"), expression.Value(&types.AttributeValueMemberSS{Value: []string{"a", "b"}})),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if _, ok := got["ss"]; ok {
						t.Error("an emptied set must be removed: DynamoDB has no empty-set representation")
					}
				},
			},
			{
				name: "multiple clauses in one expression",
				upd: expression.Set(expression.Name("s"), expression.Value(strVal("multi"))).
					Remove(expression.Name("m")).
					Add(expression.Name("n"), expression.Value(numVal("1"))),
				check: func(t *testing.T, got map[string]types.AttributeValue) {
					if got["s"].(*types.AttributeValueMemberS).Value != "multi" {
						t.Errorf("s = %v", got["s"])
					}
					if _, ok := got["m"]; ok {
						t.Error("m survived REMOVE")
					}
					if got["n"].(*types.AttributeValueMemberN).Value != "43" {
						t.Errorf("n = %v, want 43", got["n"])
					}
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				seed()
				expr := mustExpr(t, expression.NewBuilder().WithUpdate(tc.upd))
				if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:                 aws.String("Upd"),
					Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
					UpdateExpression:          expr.Update(),
					ExpressionAttributeNames:  expr.Names(),
					ExpressionAttributeValues: expr.Values(),
				}); err != nil {
					t.Fatalf("UpdateItem(%s): %v", tc.name, err)
				}
				tc.check(t, getConf(t, c, ctx, "Upd", "k"))
			})
		}
	})
}

// Case 10: actions DynamoDB rejects outright.
func TestConfUpdateRejections(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "UpdBad")
		putConf(t, c, ctx, "UpdBad", map[string]types.AttributeValue{"pk": strVal("k"), "s": strVal("hello"), "n": numVal("1")})

		cases := []struct {
			name   string
			expr   string
			values map[string]types.AttributeValue
		}{
			{"ADD on a nested path", "ADD m.b :one", map[string]types.AttributeValue{":one": numVal("1")}},
			{"DELETE on a nested path", "DELETE m.b :ss", map[string]types.AttributeValue{":ss": &types.AttributeValueMemberSS{Value: []string{"a"}}}},
			{"SET on the partition key", "SET pk = :v", map[string]types.AttributeValue{":v": strVal("other")}},
			{"REMOVE of the partition key", "REMOVE pk", nil},
			{"ADD to the partition key", "ADD pk :one", map[string]types.AttributeValue{":one": numVal("1")}},
			{"overlapping paths", "SET s = :v REMOVE s", map[string]types.AttributeValue{":v": strVal("x")}},
			{"duplicate clause keyword", "SET s = :v SET n = :one", map[string]types.AttributeValue{":v": strVal("x"), ":one": numVal("1")}},
			{"arithmetic on a non-number", "SET n = s + :one", map[string]types.AttributeValue{":one": numVal("1")}},
			{"ADD a number onto a string", "ADD s :one", map[string]types.AttributeValue{":one": numVal("1")}},
			{"malformed expression", "SET s", nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:                 aws.String("UpdBad"),
					Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
					UpdateExpression:          aws.String(tc.expr),
					ExpressionAttributeValues: tc.values,
				})
				asValidation(t, err, tc.expr)
			})
		}
	})
}

// Case 11: all five ReturnValues modes on UpdateItem.
func TestConfUpdateReturnValues(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "UpdRV")

		seed := func() {
			t.Helper()
			putConf(t, c, ctx, "UpdRV", map[string]types.AttributeValue{
				"pk": strVal("k"), "s": strVal("old"), "other": strVal("keep"),
			})
		}

		setS := mustExpr(t, expression.NewBuilder().
			WithUpdate(expression.Set(expression.Name("s"), expression.Value(strVal("new")))))
		removeS := mustExpr(t, expression.NewBuilder().
			WithUpdate(expression.Remove(expression.Name("s"))))

		cases := []struct {
			name  string
			mode  types.ReturnValue
			want  map[string]string
			empty bool
		}{
			{"NONE", types.ReturnValueNone, nil, true},
			{"ALL_OLD", types.ReturnValueAllOld, map[string]string{"pk": "k", "s": "old", "other": "keep"}, false},
			{"ALL_NEW", types.ReturnValueAllNew, map[string]string{"pk": "k", "s": "new", "other": "keep"}, false},
			{"UPDATED_OLD", types.ReturnValueUpdatedOld, map[string]string{"s": "old"}, false},
			{"UPDATED_NEW", types.ReturnValueUpdatedNew, map[string]string{"s": "new"}, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				seed()
				out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:                 aws.String("UpdRV"),
					Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
					UpdateExpression:          setS.Update(),
					ExpressionAttributeNames:  setS.Names(),
					ExpressionAttributeValues: setS.Values(),
					ReturnValues:              tc.mode,
				})
				if err != nil {
					t.Fatalf("UpdateItem: %v", err)
				}
				if tc.empty {
					if len(out.Attributes) != 0 {
						t.Errorf("Attributes = %v, want none", out.Attributes)
					}
					return
				}
				if len(out.Attributes) != len(tc.want) {
					t.Fatalf("Attributes = %v, want %v", out.Attributes, tc.want)
				}
				for k, want := range tc.want {
					v, ok := out.Attributes[k]
					if !ok {
						t.Errorf("Attributes is missing %s", k)
						continue
					}
					if got := v.(*types.AttributeValueMemberS).Value; got != want {
						t.Errorf("Attributes[%s] = %q, want %q", k, got, want)
					}
				}
			})
		}

		// UPDATED_NEW omits an attribute the update removed.
		seed()
		out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                aws.String("UpdRV"),
			Key:                      map[string]types.AttributeValue{"pk": strVal("k")},
			UpdateExpression:         removeS.Update(),
			ExpressionAttributeNames: removeS.Names(),
			ReturnValues:             types.ReturnValueUpdatedNew,
		})
		if err != nil {
			t.Fatalf("UpdateItem REMOVE: %v", err)
		}
		if len(out.Attributes) != 0 {
			t.Errorf("Attributes = %v, want none after a REMOVE", out.Attributes)
		}

		// ALL_OLD against an absent key returns nothing.
		setAbsent := mustExpr(t, expression.NewBuilder().
			WithUpdate(expression.Set(expression.Name("s"), expression.Value(strVal("x")))))
		out, err = c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String("UpdRV"),
			Key:                       map[string]types.AttributeValue{"pk": strVal("absent")},
			UpdateExpression:          setAbsent.Update(),
			ExpressionAttributeNames:  setAbsent.Names(),
			ExpressionAttributeValues: setAbsent.Values(),
			ReturnValues:              types.ReturnValueAllOld,
		})
		if err != nil {
			t.Fatalf("UpdateItem on an absent key: %v", err)
		}
		if len(out.Attributes) != 0 {
			t.Errorf("Attributes = %v, want none for an absent item", out.Attributes)
		}
	})
}

// Case 12: ReturnValuesOnConditionCheckFailure populates the exception's Item.
func TestConfReturnValuesOnConditionCheckFailure(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "RVCCF")
		putConf(t, c, ctx, "RVCCF", map[string]types.AttributeValue{"pk": strVal("k"), "s": strVal("stored")})

		t.Run("UpdateItem", func(t *testing.T) {
			// The condition and update share one builder; the "s" name is
			// deduplicated and a single unified value map is emitted.
			expr := mustExpr(t, expression.NewBuilder().
				WithCondition(expression.Name("s").Equal(expression.Value(strVal("nope")))).
				WithUpdate(expression.Set(expression.Name("s"), expression.Value(strVal("new")))))
			_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                           aws.String("RVCCF"),
				Key:                                 map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:                    expr.Update(),
				ConditionExpression:                 expr.Condition(),
				ExpressionAttributeNames:            expr.Names(),
				ExpressionAttributeValues:           expr.Values(),
				ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
			})
			e := asConditionalCheckFailed(t, err, "UpdateItem with an unsatisfied condition")
			if e == nil {
				return
			}
			if got, ok := e.Item["s"]; !ok || got.(*types.AttributeValueMemberS).Value != "stored" {
				t.Errorf("exception Item = %v, want the pre-write item", e.Item)
			}
		})

		t.Run("PutItem", func(t *testing.T) {
			expr := mustExpr(t, expression.NewBuilder().
				WithCondition(expression.AttributeNotExists(expression.Name("pk"))))
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                           aws.String("RVCCF"),
				Item:                                map[string]types.AttributeValue{"pk": strVal("k"), "s": strVal("new")},
				ConditionExpression:                 expr.Condition(),
				ExpressionAttributeNames:            expr.Names(),
				ExpressionAttributeValues:           expr.Values(),
				ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
			})
			e := asConditionalCheckFailed(t, err, "PutItem with an unsatisfied condition")
			if e == nil {
				return
			}
			if got, ok := e.Item["s"]; !ok || got.(*types.AttributeValueMemberS).Value != "stored" {
				t.Errorf("exception Item = %v, want the pre-write item", e.Item)
			}
		})

		t.Run("omitted by default", func(t *testing.T) {
			expr := mustExpr(t, expression.NewBuilder().
				WithCondition(expression.AttributeNotExists(expression.Name("pk"))))
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String("RVCCF"),
				Item:                      map[string]types.AttributeValue{"pk": strVal("k"), "s": strVal("new")},
				ConditionExpression:       expr.Condition(),
				ExpressionAttributeNames:  expr.Names(),
				ExpressionAttributeValues: expr.Values(),
			})
			e := asConditionalCheckFailed(t, err, "PutItem without ReturnValuesOnConditionCheckFailure")
			if e != nil && len(e.Item) != 0 {
				t.Errorf("exception Item = %v, want none when the request did not ask for it", e.Item)
			}
		})
	})
}

// --- M2 pass 3: validation ordering ---

// Case 13: a request that is invalid in two ways reports the failure DynamoDB
// reports. The service validates the table first, so a missing table beats a
// malformed expression, an unsupported ReturnValues mode, and an unused
// substitution — all of which would otherwise be ValidationException.
func TestConfValidationOrderingTableFirst(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		key := map[string]types.AttributeValue{"pk": strVal("k")}
		item := map[string]types.AttributeValue{"pk": strVal("k")}
		const missing = "NoSuchTableHere"

		t.Run("PutItem malformed condition", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:           aws.String(missing),
				Item:                item,
				ConditionExpression: aws.String("pk ="),
			})
			asResourceNotFound(t, err, "missing table + malformed condition")
		})

		t.Run("DeleteItem malformed condition", func(t *testing.T) {
			_, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName:           aws.String(missing),
				Key:                 key,
				ConditionExpression: aws.String("pk ="),
			})
			asResourceNotFound(t, err, "missing table + malformed condition")
		})

		t.Run("UpdateItem malformed update", func(t *testing.T) {
			_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:        aws.String(missing),
				Key:              key,
				UpdateExpression: aws.String("SET a"),
			})
			asResourceNotFound(t, err, "missing table + malformed update")
		})

		t.Run("UpdateItem unsupported ReturnValues", func(t *testing.T) {
			_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String(missing),
				Key:                       key,
				UpdateExpression:          aws.String("SET marker = :v"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
				ReturnValues:              types.ReturnValue("BOGUS"),
			})
			asResourceNotFound(t, err, "missing table + bogus ReturnValues")
		})

		t.Run("UpdateItem unused substitution", func(t *testing.T) {
			_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String(missing),
				Key:                       key,
				UpdateExpression:          aws.String("REMOVE marker"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":unused": strVal("x")},
			})
			asResourceNotFound(t, err, "missing table + unused substitution")
		})

		t.Run("PutItem unsupported ReturnValues", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:    aws.String(missing),
				Item:         item,
				ReturnValues: types.ReturnValueAllNew,
			})
			asResourceNotFound(t, err, "missing table + ALL_NEW on PutItem")
		})
	})
}

// Case 14: a pathologically nested condition is rejected rather than crashing.
// The engine caps grammar recursion (expr.maxParseDepth); the reference
// rejects the same input for exceeding its operator and expression-size
// limits. Either way the caller sees a ValidationException, and neither target
// dies with a stack overflow.
func TestConfPathologicalNesting(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DeepNest")
		const levels = 20000
		cond := strings.Repeat("pk = :v OR (", levels) + "pk = :v" + strings.Repeat(")", levels)
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("DeepNest"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("k")},
		})
		asValidation(t, err, "condition nested 20000 levels deep")

		// A modestly nested condition stays valid on both targets. Seed the
		// item first so the condition itself holds.
		putConf(t, c, ctx, "DeepNest", map[string]types.AttributeValue{"pk": strVal("k")})
		const shallow = 20
		ok := strings.Repeat("pk = :v OR (", shallow) + "pk = :v" + strings.Repeat(")", shallow)
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("DeepNest"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String(ok),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("k")},
		}); err != nil {
			t.Errorf("condition nested %d levels: err = %v, want success", shallow, err)
		}
	})
}

// Case 15: document paths that mix bare segments with #name substitutions.
// The SDK expression builder aliases every segment, so builder-generated cases
// never exercise the hand-written shapes callers actually send: a bare parent
// with an aliased child, an aliased parent with a bare child, and an alias
// covering only the reserved word in a path.
func TestConfMixedAliasBarePaths(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "MixedPath")
		seed := func() {
			putConf(t, c, ctx, "MixedPath", map[string]types.AttributeValue{
				"pk": strVal("k"),
				"m": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
					"child": strVal("v"),
					"data":  strVal("d"), // reserved word: only valid via an alias
				}},
				"l": &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("a"), strVal("b")}},
			})
		}

		conds := []struct {
			name, cond string
			names      map[string]string
			vals       map[string]types.AttributeValue
		}{
			{"bare parent, aliased child", "attribute_exists(m.#c)", map[string]string{"#c": "child"}, nil},
			{"aliased parent, bare child", "attribute_exists(#m.child)", map[string]string{"#m": "m"}, nil},
			{"fully bare nested path", "m.child = :v", nil, map[string]types.AttributeValue{":v": strVal("v")}},
			{"alias covers only the reserved word", "m.#d = :d", map[string]string{"#d": "data"}, map[string]types.AttributeValue{":d": strVal("d")}},
			{"bare list index", "l[1] = :b", nil, map[string]types.AttributeValue{":b": strVal("b")}},
			{"aliased list index", "#l[1] = :b", map[string]string{"#l": "l"}, map[string]types.AttributeValue{":b": strVal("b")}},
			{"function over a mixed path", "begins_with(#m.child, :p)", map[string]string{"#m": "m"}, map[string]types.AttributeValue{":p": strVal("v")}},
		}
		for _, tc := range conds {
			t.Run(tc.name, func(t *testing.T) {
				seed()
				_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
					TableName:                 aws.String("MixedPath"),
					Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
					ConditionExpression:       aws.String(tc.cond),
					ExpressionAttributeNames:  tc.names,
					ExpressionAttributeValues: tc.vals,
				})
				if err != nil {
					t.Errorf("%s: err = %v, want the condition to hold", tc.cond, err)
				}
			})
		}

		// Updates through the same shapes, verified by reading the item back
		// rather than through ReturnValues.
		t.Run("SET aliased parent, bare child", func(t *testing.T) {
			seed()
			if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("MixedPath"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("SET #m.child = :n"),
				ExpressionAttributeNames:  map[string]string{"#m": "m"},
				ExpressionAttributeValues: map[string]types.AttributeValue{":n": strVal("n2")},
			}); err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			wantNestedString(t, getConf(t, c, ctx, "MixedPath", "k"), "m", "child", "n2")
		})

		t.Run("SET bare parent, aliased child", func(t *testing.T) {
			seed()
			if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("MixedPath"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("SET m.#c = :n"),
				ExpressionAttributeNames:  map[string]string{"#c": "child"},
				ExpressionAttributeValues: map[string]types.AttributeValue{":n": strVal("n3")},
			}); err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			wantNestedString(t, getConf(t, c, ctx, "MixedPath", "k"), "m", "child", "n3")
		})

		t.Run("REMOVE a reserved-word child via alias", func(t *testing.T) {
			seed()
			if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                aws.String("MixedPath"),
				Key:                      map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:         aws.String("REMOVE m.#d"),
				ExpressionAttributeNames: map[string]string{"#d": "data"},
			}); err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			m, ok := getConf(t, c, ctx, "MixedPath", "k")["m"].(*types.AttributeValueMemberM)
			if !ok {
				t.Fatalf("m is not a map")
			}
			if _, still := m.Value["data"]; still {
				t.Errorf("m.data = %v, want it removed", m.Value["data"])
			}
			wantNestedString(t, getConf(t, c, ctx, "MixedPath", "k"), "m", "child", "v")
		})

		t.Run("SET through an aliased list index", func(t *testing.T) {
			seed()
			if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("MixedPath"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("SET #l[0] = :n"),
				ExpressionAttributeNames:  map[string]string{"#l": "l"},
				ExpressionAttributeValues: map[string]types.AttributeValue{":n": strVal("z")},
			}); err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			wantStrList(t, getConf(t, c, ctx, "MixedPath", "k")["l"], []string{"z", "b"}, "SET #l[0]")
		})
	})
}

// wantNestedString asserts item[attr][key] is the string want.
func wantNestedString(t *testing.T, item map[string]types.AttributeValue, attr, key, want string) {
	t.Helper()
	m, ok := item[attr].(*types.AttributeValueMemberM)
	if !ok {
		t.Errorf("%s = %T, want a map", attr, item[attr])
		return
	}
	s, ok := m.Value[key].(*types.AttributeValueMemberS)
	if !ok {
		t.Errorf("%s.%s = %T, want a string", attr, key, m.Value[key])
		return
	}
	if s.Value != want {
		t.Errorf("%s.%s = %q, want %q", attr, key, s.Value, want)
	}
}

// The cases below pin behaviors originally captured as divergences from the
// adapter and since fixed. Every expectation was measured against
// dynamodb-local 3.3.1 first.

// wantStrList asserts that av is a list of strings equal to want.
func wantStrList(t *testing.T, av types.AttributeValue, want []string, msg string) {
	t.Helper()
	l, ok := av.(*types.AttributeValueMemberL)
	if !ok {
		t.Errorf("%s: value = %T, want list", msg, av)
		return
	}
	if len(l.Value) != len(want) {
		t.Errorf("%s: list = %s, want %v", msg, renderStrList(l.Value), want)
		return
	}
	for i, e := range l.Value {
		s, ok := e.(*types.AttributeValueMemberS)
		if !ok || s.Value != want[i] {
			t.Errorf("%s: list = %s, want %v", msg, renderStrList(l.Value), want)
			return
		}
	}
}

func renderStrList(l []types.AttributeValue) string {
	out := "["
	for i, e := range l {
		if i > 0 {
			out += " "
		}
		if s, ok := e.(*types.AttributeValueMemberS); ok {
			out += s.Value
		} else {
			out += "?"
		}
	}
	return out + "]"
}

func strList(vals ...string) types.AttributeValue {
	l := make([]types.AttributeValue, 0, len(vals))
	for _, v := range vals {
		l = append(l, strVal(v))
	}
	return &types.AttributeValueMemberL{Value: l}
}

// SET through a document path whose parent does not exist is a
// ValidationException: only the *final* path segment may be created.
func TestConfSetThroughMissingParent(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivParent")

		cases := []struct {
			name string
			seed map[string]types.AttributeValue
			expr string
			ok   bool // true = both targets accept (control case)
		}{
			{
				name: "missing top-level parent",
				seed: map[string]types.AttributeValue{"pk": strVal("k")},
				expr: "SET box.leaf = :v",
			},
			{
				name: "missing intermediate under existing map",
				seed: map[string]types.AttributeValue{"pk": strVal("k"), "m": &types.AttributeValueMemberM{
					Value: map[string]types.AttributeValue{"child": strVal("v")}}},
				expr: "SET m.nest.leaf = :v",
			},
			{
				name: "three levels missing",
				seed: map[string]types.AttributeValue{"pk": strVal("k")},
				expr: "SET box.nest.child.leaf = :v",
			},
			{
				name: "if_not_exists does not excuse a missing parent",
				seed: map[string]types.AttributeValue{"pk": strVal("k")},
				expr: "SET box.leaf = if_not_exists(box.leaf, :v)",
			},
			{
				// Control: the final segment may be created under an existing map.
				name: "leaf under existing map is allowed",
				seed: map[string]types.AttributeValue{"pk": strVal("k"), "m": &types.AttributeValueMemberM{
					Value: map[string]types.AttributeValue{"child": strVal("v")}}},
				expr: "SET m.leaf = :v",
				ok:   true,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				putConf(t, c, ctx, "DivParent", tc.seed)
				_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:                 aws.String("DivParent"),
					Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
					UpdateExpression:          aws.String(tc.expr),
					ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("z")},
				})
				if tc.ok {
					if err != nil {
						t.Errorf("%s: err = %v, want success", tc.expr, err)
					}
					return
				}
				asValidation(t, err, tc.expr)
			})
		}
	})
}

// SET at a list index past the end of the list clamps to an append.
func TestConfSetListIndexPastEnd(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivIndex")

		cases := []struct {
			name string
			expr string
			want []string
		}{
			{"index far past end appends", "SET l[5] = :v", []string{"a", "z"}},
			{"index one past end appends", "SET l[1] = :v", []string{"a", "z"}},
			{"index in range overwrites", "SET l[0] = :v", []string{"z"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				putConf(t, c, ctx, "DivIndex", map[string]types.AttributeValue{
					"pk": strVal("k"), "l": strList("a"),
				})
				_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:                 aws.String("DivIndex"),
					Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
					UpdateExpression:          aws.String(tc.expr),
					ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("z")},
				})
				if err != nil {
					t.Fatalf("%s: err = %v, want success", tc.expr, err)
				}
				wantStrList(t, getConf(t, c, ctx, "DivIndex", "k")["l"], tc.want, tc.expr)
			})
		}
	})
}

// Several list-index REMOVEs in one expression resolve every index against
// the ORIGINAL list; earlier removals do not shift later indexes.
func TestConfMultiRemoveListIndex(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivRemove")

		cases := []struct {
			name string
			expr string
			want []string
		}{
			{"adjacent ascending", "REMOVE l[0], l[1]", []string{"c", "d"}},
			{"straddling ascending", "REMOVE l[0], l[2]", []string{"b", "d"}},
			{"descending order", "REMOVE l[1], l[0]", []string{"c", "d"}},
			{"three at once", "REMOVE l[0], l[1], l[2]", []string{"d"}},
			{"out-of-range index ignored", "REMOVE l[0], l[9]", []string{"b", "c", "d"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				putConf(t, c, ctx, "DivRemove", map[string]types.AttributeValue{
					"pk": strVal("k"), "l": strList("a", "b", "c", "d"),
				})
				_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
					TableName:        aws.String("DivRemove"),
					Key:              map[string]types.AttributeValue{"pk": strVal("k")},
					UpdateExpression: aws.String(tc.expr),
				})
				if err != nil {
					t.Fatalf("%s: err = %v, want success", tc.expr, err)
				}
				wantStrList(t, getConf(t, c, ctx, "DivRemove", "k")["l"], tc.want, tc.expr)
			})
		}
	})
}

// UPDATED_NEW: a REMOVE-only update contributes no attributes, while
// SET/ADD/DELETE contribute the whole top-level attribute.
func TestConfUpdatedNewAfterNestedRemove(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivUpdNew")
		seed := func() {
			putConf(t, c, ctx, "DivUpdNew", map[string]types.AttributeValue{
				"pk": strVal("k"),
				"m": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
					"a": strVal("1"), "b": strVal("2"),
				}},
			})
		}

		t.Run("nested REMOVE returns no attributes", func(t *testing.T) {
			seed()
			out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:        aws.String("DivUpdNew"),
				Key:              map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression: aws.String("REMOVE m.a"),
				ReturnValues:     types.ReturnValueUpdatedNew,
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			if len(out.Attributes) != 0 {
				t.Errorf("UPDATED_NEW = %v, want no attributes", out.Attributes)
			}
		})

		// Control: UPDATED_OLD after a nested REMOVE does report the attribute.
		t.Run("nested REMOVE reports UPDATED_OLD", func(t *testing.T) {
			seed()
			out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:        aws.String("DivUpdNew"),
				Key:              map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression: aws.String("REMOVE m.a"),
				ReturnValues:     types.ReturnValueUpdatedOld,
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			if _, ok := out.Attributes["m"]; !ok {
				t.Errorf("UPDATED_OLD = %v, want attribute m", out.Attributes)
			}
		})

		// Control: a SET in the same expression does contribute the attribute.
		t.Run("SET alongside REMOVE reports the attribute", func(t *testing.T) {
			seed()
			out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("DivUpdNew"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("SET m.x = :v REMOVE m.a"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("3")},
				ReturnValues:              types.ReturnValueUpdatedNew,
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			if _, ok := out.Attributes["m"]; !ok {
				t.Errorf("UPDATED_NEW = %v, want attribute m", out.Attributes)
			}
		})

		// A REMOVE elsewhere in the expression must not drag its own attribute
		// into UPDATED_NEW just because a different attribute was SET.
		t.Run("REMOVE of another attribute stays out of UPDATED_NEW", func(t *testing.T) {
			seed()
			out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("DivUpdNew"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("SET top = :v REMOVE m.a"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("t2")},
				ReturnValues:              types.ReturnValueUpdatedNew,
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			if _, ok := out.Attributes["top"]; !ok {
				t.Errorf("UPDATED_NEW = %v, want attribute top", out.Attributes)
			}
			if _, ok := out.Attributes["m"]; ok {
				t.Errorf("UPDATED_NEW = %v, want no attribute m (only REMOVE touched it)", out.Attributes)
			}
		})
	})
}

// UPDATED_OLD keys off whether the touched PATH existed, not whether the
// top-level attribute existed. A SET that creates a new nested path reports
// nothing, even though its top-level attribute was already present.
func TestConfUpdatedOldNewNestedPath(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivUpdOld")
		putConf(t, c, ctx, "DivUpdOld", map[string]types.AttributeValue{
			"pk": strVal("k"),
			"m": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
				"a": strVal("1"), "b": strVal("2"),
			}},
		})
		out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String("DivUpdOld"),
			Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
			UpdateExpression:          aws.String("SET m.c = :v"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("3")},
			ReturnValues:              types.ReturnValueUpdatedOld,
		})
		if err != nil {
			t.Fatalf("UpdateItem: %v", err)
		}
		if len(out.Attributes) != 0 {
			t.Errorf("UPDATED_OLD = %v, want no attributes (m.c did not exist)", out.Attributes)
		}
	})
}

// DELETE contributes to UPDATED_NEW only while the attribute survives:
// emptying a set removes it, and a removed attribute is not reported.
func TestConfUpdatedNewAfterDelete(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivUpdDel")
		seed := func() {
			putConf(t, c, ctx, "DivUpdDel", map[string]types.AttributeValue{
				"pk": strVal("k"),
				"ss": &types.AttributeValueMemberSS{Value: []string{"x", "y"}},
			})
		}

		t.Run("DELETE emptying the set reports nothing", func(t *testing.T) {
			seed()
			out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("DivUpdDel"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("DELETE ss :all"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":all": &types.AttributeValueMemberSS{Value: []string{"x", "y"}}},
				ReturnValues:              types.ReturnValueUpdatedNew,
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			if len(out.Attributes) != 0 {
				t.Errorf("UPDATED_NEW = %v, want no attributes (ss was emptied away)", out.Attributes)
			}
		})

		// Control: a partial DELETE leaves the set, which is reported.
		t.Run("partial DELETE reports the surviving set", func(t *testing.T) {
			seed()
			out, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("DivUpdDel"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("DELETE ss :one"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":one": &types.AttributeValueMemberSS{Value: []string{"x"}}},
				ReturnValues:              types.ReturnValueUpdatedNew,
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			if _, ok := out.Attributes["ss"]; !ok {
				t.Errorf("UPDATED_NEW = %v, want attribute ss", out.Attributes)
			}
		})
	})
}

// A present-but-empty substitution map alongside an expression is a
// ValidationException. With no expression at all the SDK omits the empty map
// from the payload, so both targets accept the request. Only the adapter can
// make this distinction; ddb's input structs use plain maps, where nil and
// empty are indistinguishable.
func TestConfEmptySubstitutionMaps(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivEmptySub")
		item := map[string]types.AttributeValue{"pk": strVal("k")}
		putConf(t, c, ctx, "DivEmptySub", item)

		t.Run("empty names with a condition", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                aws.String("DivEmptySub"),
				Item:                     item,
				ConditionExpression:      aws.String("attribute_exists(pk)"),
				ExpressionAttributeNames: map[string]string{},
			})
			asValidation(t, err, "empty ExpressionAttributeNames")
		})

		t.Run("empty values with a condition", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String("DivEmptySub"),
				Item:                      item,
				ConditionExpression:       aws.String("attribute_exists(pk)"),
				ExpressionAttributeValues: map[string]types.AttributeValue{},
			})
			asValidation(t, err, "empty ExpressionAttributeValues")
		})

		t.Run("empty names with an update expression", func(t *testing.T) {
			_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("DivEmptySub"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("SET marker = :v"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
				ExpressionAttributeNames:  map[string]string{},
			})
			asValidation(t, err, "empty ExpressionAttributeNames on update")
		})

		// Control: with no expression at all the SDK drops the empty map, so
		// both targets accept the request.
		t.Run("empty names with no expression is accepted", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                aws.String("DivEmptySub"),
				Item:                     item,
				ExpressionAttributeNames: map[string]string{},
			})
			if err != nil {
				t.Errorf("empty names without an expression: err = %v, want success", err)
			}
		})
	})
}

// Empty sets are rejected at input validation — both on PutItem and as an
// ADD operand.
func TestConfEmptySets(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivEmptySet")

		t.Run("PutItem with an empty string set", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName: aws.String("DivEmptySet"),
				Item: map[string]types.AttributeValue{
					"pk": strVal("k"),
					"ss": &types.AttributeValueMemberSS{Value: []string{}},
				},
			})
			asValidation(t, err, "PutItem empty SS")
		})

		t.Run("ADD of an empty string set", func(t *testing.T) {
			putConf(t, c, ctx, "DivEmptySet", map[string]types.AttributeValue{"pk": strVal("k2")})
			_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("DivEmptySet"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k2")},
				UpdateExpression:          aws.String("ADD ss :e"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":e": &types.AttributeValueMemberSS{Value: []string{}}},
			})
			asValidation(t, err, "ADD empty SS")
		})
	})
}

// BETWEEN bounds of different types are a ValidationException. (Contrast: a
// cross-type ordered *comparison* evaluates false rather than erroring.)
func TestConfBetweenMixedBoundTypes(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivBetween")
		putConf(t, c, ctx, "DivBetween", map[string]types.AttributeValue{
			"pk": strVal("k"), "n": numVal("5"),
		})
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String("DivBetween"),
			Item:                map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression: aws.String("n BETWEEN :s AND :n"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":s": strVal("a"), ":n": numVal("9"),
			},
		})
		asValidation(t, err, "BETWEEN with S lower bound and N upper bound")
	})
}

// A list index with a leading zero ("l[01]") is a syntax error.
func TestConfLeadingZeroListIndex(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivLeadZero")
		putConf(t, c, ctx, "DivLeadZero", map[string]types.AttributeValue{
			"pk": strVal("k"), "l": strList("a", "b"),
		})
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("DivLeadZero"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String("l[01] = :b"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":b": strVal("b")},
		})
		asValidation(t, err, "list index with a leading zero")
	})
}

// ReturnValues is case-sensitive: only the exact upper-case enum spellings
// are accepted.
func TestConfReturnValuesCaseSensitivity(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DivRVCase")
		putConf(t, c, ctx, "DivRVCase", map[string]types.AttributeValue{"pk": strVal("k")})

		t.Run("PutItem all_old", func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:    aws.String("DivRVCase"),
				Item:         map[string]types.AttributeValue{"pk": strVal("k")},
				ReturnValues: types.ReturnValue("all_old"),
			})
			asValidation(t, err, `ReturnValues "all_old"`)
		})

		t.Run("UpdateItem all_new", func(t *testing.T) {
			_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("DivRVCase"),
				Key:                       map[string]types.AttributeValue{"pk": strVal("k")},
				UpdateExpression:          aws.String("SET marker = :v"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
				ReturnValues:              types.ReturnValue("all_new"),
			})
			asValidation(t, err, `ReturnValues "all_new"`)
		})

		t.Run("DeleteItem AllOld mixed case", func(t *testing.T) {
			_, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName:    aws.String("DivRVCase"),
				Key:          map[string]types.AttributeValue{"pk": strVal("k")},
				ReturnValues: types.ReturnValue("AllOld"),
			})
			asValidation(t, err, `ReturnValues "AllOld"`)
		})
	})
}
