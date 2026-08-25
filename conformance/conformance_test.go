package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
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

	"github.com/quells-bot/ddb-sqlite/pkg/ddb-sqlite"
)

// api is the minimal interface (exact SDK method signatures) both *dynamodb.Client
// and *ddbsqlite.Adapter satisfy. The harness is parameterized by it so the
// same cases run against the adapter and dynamodb-local.
type api interface {
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	ListTables(ctx context.Context, params *dynamodb.ListTablesInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error)
	DeleteTable(ctx context.Context, params *dynamodb.DeleteTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error)
	UpdateTable(ctx context.Context, params *dynamodb.UpdateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateTableOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	BatchGetItem(ctx context.Context, params *dynamodb.BatchGetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error)
	UpdateTimeToLive(ctx context.Context, params *dynamodb.UpdateTimeToLiveInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
	DescribeTimeToLive(ctx context.Context, params *dynamodb.DescribeTimeToLiveInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
}

type confTarget struct {
	name string
	ctor func(t *testing.T) (api, func())
}

var confTargets = []confTarget{
	{"adapter", newAdapterTarget},
	{"dynamodb-local", newLocalTarget},
}

func runConformance(t *testing.T, fn func(*testing.T, api)) {
	for _, tg := range confTargets {
		tg := tg
		t.Run(tg.name, func(t *testing.T) {
			c, cleanup := tg.ctor(t)
			defer cleanup()
			fn(t, c)
		})
	}
}

func newAdapterTarget(t *testing.T) (api, func()) {
	a, err := ddbsqlite.Open(context.Background(), ":memory:")
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

// --- dynamodb-local target ---

// dynamodb-local target state, owned by TestMain. The container is shared
// across the whole test binary; newLocalTarget hands out the shared client
// and returns a cleanup that purges all tables so each test starts clean.
var (
	localClient     api
	localPool       dockertest.ClosablePool
	localResource   dockertest.ClosableResource
	localSkipReason string
)

func TestMain(m *testing.M) {
	if err := setupLocalTarget(context.Background()); err != nil {
		localSkipReason = err.Error()
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

// batchPut/batchDel build BatchWriteItem request entries for conformance cases.
func batchPut(item map[string]types.AttributeValue) types.WriteRequest {
	return types.WriteRequest{PutRequest: &types.PutRequest{Item: item}}
}

func batchDel(key map[string]types.AttributeValue) types.WriteRequest {
	return types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: key}}
}

// mustCreateComposite creates a table with a composite primary key (pk HASH S,
// sk RANGE N) for Query/Scan conformance cases.
func mustCreateComposite(t *testing.T, c api, ctx context.Context, name string) {
	t.Helper()
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(name),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeN},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable %q: %v", name, err)
	}
}

// mustCreateGsiTable builds the GSI conformance table: pk HASH S, sk RANGE S,
// with three GSIs:
//   - gsi-all:   gsi_pk HASH S, gsi_sk RANGE S, ALL
//   - gsi-keys:  gsi_pk HASH S, (no sort),       KEYS_ONLY
//   - gsi-incl:  gsi_pk HASH S, gsi_sk RANGE S, INCLUDE [proj1, proj2]
func mustCreateGsiTable(t *testing.T, c api, ctx context.Context, name string) {
	t.Helper()
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(name),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("gsi_pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("gsi_sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String("gsi-all"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("gsi_pk"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("gsi_sk"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
			{
				IndexName:  aws.String("gsi-keys"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gsi_pk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			},
			{
				IndexName: aws.String("gsi-incl"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("gsi_pk"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("gsi_sk"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeInclude, NonKeyAttributes: []string{"proj1", "proj2"}},
			},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable GSI %q: %v", name, err)
	}
}

// seedGsiConformance puts the five seed items into the table.
func seedGsiConformance(t *testing.T, c api, ctx context.Context, table string) {
	t.Helper()
	items := []map[string]types.AttributeValue{
		{"pk": sv("A"), "sk": sv("a"), "gsi_pk": sv("G1"), "gsi_sk": sv("s1"), "proj1": sv("foo"), "proj2": sv("bar"), "extra": sv("baz")},
		{"pk": sv("B"), "sk": sv("b"), "gsi_pk": sv("G1"), "gsi_sk": sv("s2"), "proj1": sv("qux"), "extra": sv("quux")},
		{"pk": sv("C"), "sk": sv("c"), "gsi_pk": sv("G1"), "gsi_sk": sv("s1")},
		{"pk": sv("D"), "sk": sv("d")},
		{"pk": sv("E"), "sk": sv("e"), "gsi_pk": sv("G2"), "gsi_sk": sv("s3"), "proj1": sv("alpha")},
	}
	for _, it := range items {
		putConf(t, c, ctx, table, it)
	}
}

// itemAttrNamesConf returns sorted attribute names of a conformance item.
func itemAttrNamesConf(item map[string]types.AttributeValue) []string {
	names := make([]string, 0, len(item))
	for k := range item {
		names = append(names, k)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

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

func asResourceInUse(t *testing.T, err error, msg string) {
	t.Helper()
	var e *types.ResourceInUseException
	if !errors.As(err, &e) {
		t.Errorf("%s: err = %v, want ResourceInUseException", msg, err)
	}
}

func asLimitExceeded(t *testing.T, err error, msg string) {
	t.Helper()
	var e *types.LimitExceededException
	if !errors.As(err, &e) {
		t.Errorf("%s: err = %v, want LimitExceededException", msg, err)
	}
}

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

// ListTables with Limit == table count returns all tables and does NOT set LastEvaluatedTableName.
func TestConfListTablesLimitEqualsCount(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		for _, n := range []string{"alpha", "bravo", "charlie"} {
			mustCreate(t, c, ctx, n)
		}
		out, err := c.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(3)})
		if err != nil {
			t.Fatalf("ListTables: %v", err)
		}
		if len(out.TableNames) != 3 {
			t.Errorf("TableNames = %v, want all 3", out.TableNames)
		}
		if out.LastEvaluatedTableName != nil {
			t.Errorf("LastEvaluatedTableName = %q, want nil when Limit == table count",
				aws.ToString(out.LastEvaluatedTableName))
		}
	})
}

// A GSI named identically to its table is accepted at CreateTable and
// is usable (no collision validation exists or is needed).
func TestConfGsiSameNameAsTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("SameName"),
			KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gk"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("SameName"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			}},
			BillingMode: types.BillingModePayPerRequest,
		})
		if err != nil {
			t.Fatalf("CreateTable with same-name GSI: %v", err)
		}

		putConf(t, c, ctx, "SameName", map[string]types.AttributeValue{"pk": strVal("k"), "gk": strVal("g")})
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(
			expression.Key("gk").Equal(expression.Value("g"))))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("SameName"),
			IndexName:                 aws.String("SameName"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query same-name GSI: %v", err)
		}
		if out.Count != 1 {
			t.Errorf("Count = %d, want 1", out.Count)
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

		// Exactly 409600 bytes by AWS accounting: accepted.
		// {pk:"k", big:S(409594)} = 2+1 + 3+409594 = 409600.
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Tbl"), Item: map[string]types.AttributeValue{
			"pk":  strVal("k"),
			"big": strVal(strings.Repeat("x", 409594)),
		}})
		if err != nil {
			t.Fatalf("exact-boundary item: err = %v, want nil", err)
		}

		// 409601 bytes: rejected.
		_, err = c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Tbl"), Item: map[string]types.AttributeValue{
			"pk":  strVal("k2"),
			"big": strVal(strings.Repeat("x", 409595)),
		}})
		asValidation(t, err, "item size")
	})
}

func TestConfPutItemNumberSize(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "NumSz")

		// A 38-significant-digit number contributes 20 bytes (ceil(38/2)+1).
		// Fill the rest with a string to hit exactly 409600:
		// "pk"=2+"k"=1 + "n"=1+20 + "pad"=3+str = 409600 -> str=409573.
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("NumSz"), Item: map[string]types.AttributeValue{
			"pk":  strVal("k"),
			"n":   &types.AttributeValueMemberN{Value: "99999999999999999999999999999999999999"},
			"pad": strVal(strings.Repeat("x", 409573)),
		}})
		if err != nil {
			t.Fatalf("38-digit number exact boundary: err = %v", err)
		}

		// One more byte of string: 409601 -> rejected.
		_, err = c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("NumSz"), Item: map[string]types.AttributeValue{
			"pk":  strVal("k2"),
			"n":   &types.AttributeValueMemberN{Value: "99999999999999999999999999999999999999"},
			"pad": strVal(strings.Repeat("x", 409574)),
		}})
		asValidation(t, err, "item size")
	})
}

func TestConfPutItemDepthLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Dpth")

		// 33-level-deep nested map (2-deep base 'leaf' + 32 wraps = depth 34) -> rejected.
		inner := map[string]types.AttributeValue{"leaf": strVal("x")}
		for range 32 {
			inner = map[string]types.AttributeValue{"d": &types.AttributeValueMemberM{Value: inner}}
		}
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("Dpth"), Item: map[string]types.AttributeValue{
			"pk":   strVal("k"),
			"nest": &types.AttributeValueMemberM{Value: inner},
		}})
		asValidation(t, err, "nesting depth")
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

// seedSemItem exercises all ten attribute types.
// Cases that satisfy their condition overwrite the item, so it is re-seeded between subtests.
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

// comparator and function semantics over all ten types, missing vs
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
			// attribute is by definition not equal to anything.
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
			// evalContains' TagList branch is correct.
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

// condCase is one condition-semantics assertion observed through a PutItem
// ConditionExpression: want=true expects the put to succeed, want=false
// expects ConditionalCheckFailedException.
type condCase struct {
	name   string
	cond   string
	names  map[string]string
	values map[string]types.AttributeValue
	want   bool
}

// runCondCases evaluates each case against table by re-putting the fixture's
// key under the condition. A satisfied condition overwrites the fixture, so
// seed is restored after every success.
func runCondCases(t *testing.T, c api, ctx context.Context, table string, seed map[string]types.AttributeValue, cases []condCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:                 aws.String(table),
				Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
				ConditionExpression:       aws.String(tc.cond),
				ExpressionAttributeNames:  tc.names,
				ExpressionAttributeValues: tc.values,
			})
			if tc.want {
				if err != nil {
					t.Fatalf("condition should have been satisfied: %v", err)
				}
				putConf(t, c, ctx, table, seed)
				return
			}
			asConditionalCheckFailed(t, err, tc.name)
		})
	}
}

func TestConfNullVsMissing(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "NullMiss")
		putConf(t, c, ctx, "NullMiss", seedSemItem())

		svMap := func(s string) map[string]types.AttributeValue {
			return map[string]types.AttributeValue{":v": strVal(s)}
		}
		nullName := map[string]string{"#z": "null"} // "null" is a reserved word
		runCondCases(t, c, ctx, "NullMiss", seedSemItem(), []condCase{
			{"nested missing equality is false", "m.nope = :v", nil, svMap("z"), false},
			{"nested missing inequality is true", "m.nope <> :v", nil, svMap("z"), true},
			{"deep missing path equality is false", "nope.deep.nest = :v", nil, svMap("z"), false},
			{"scalar descent equality is false", "s.deeper = :v", nil, svMap("z"), false},
			{"list index past end equality is false", "l[5] = :v", nil, svMap("z"), false},
			{"list index past end inequality is true", "l[5] <> :v", nil, svMap("z"), true},
			{"null ordering is false", "#z < :v", nullName, svMap("z"), false},
			{"null inequality is true", "#z <> :v", nullName, svMap("z"), true},
			{"size of missing path is false", "size(nope) = :v", nil, map[string]types.AttributeValue{":v": numVal("0")}, false},
			{"contains on missing path is false", "contains(nope, :v)", nil, svMap("x"), false},
		})

		// Filters: a missing-path equality consumes read budget but returns
		// nothing; the inequality passes every scanned item.
		scan, err := c.Scan(ctx, &dynamodb.ScanInput{
			TableName:                 aws.String("NullMiss"),
			FilterExpression:          aws.String("#x = :v"),
			ExpressionAttributeNames:  map[string]string{"#x": "nope"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
		})
		if err != nil {
			t.Fatalf("Scan filter =: %v", err)
		}
		if scan.Count != 0 || scan.ScannedCount != 1 {
			t.Errorf("filter =: Count=%d ScannedCount=%d, want 0/1", scan.Count, scan.ScannedCount)
		}
		scan, err = c.Scan(ctx, &dynamodb.ScanInput{
			TableName:                 aws.String("NullMiss"),
			FilterExpression:          aws.String("#x <> :v"),
			ExpressionAttributeNames:  map[string]string{"#x": "nope"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
		})
		if err != nil {
			t.Fatalf("Scan filter <>: %v", err)
		}
		if scan.Count != 1 || scan.ScannedCount != 1 {
			t.Errorf("filter <>: Count=%d ScannedCount=%d, want 1/1", scan.Count, scan.ScannedCount)
		}

		// Projection of a NULL-valued attribute returns it as NULL.
		got, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:                aws.String("NullMiss"),
			Key:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ProjectionExpression:     aws.String("#z"),
			ExpressionAttributeNames: map[string]string{"#z": "null"},
		})
		if err != nil {
			t.Fatalf("GetItem projection: %v", err)
		}
		if _, ok := got.Item["null"].(*types.AttributeValueMemberNULL); !ok {
			t.Errorf("projected null attr = %v, want AttributeValueMemberNULL", got.Item["null"])
		}
	})
}

// type-mismatched comparisons are false (or true for <>) — never an
// error — when the *operand* is a valid scalar/set type for its operator.
func TestConfTypeMismatchComparisons(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "TypeMM")
		putConf(t, c, ctx, "TypeMM", seedSemItem())

		nullV := &types.AttributeValueMemberNULL{Value: true}
		boolT := &types.AttributeValueMemberBOOL{Value: true}
		ssBA := &types.AttributeValueMemberSS{Value: []string{"b", "a"}}
		ssAB := &types.AttributeValueMemberSS{Value: []string{"a", "b"}}
		ssA := &types.AttributeValueMemberSS{Value: []string{"a"}}
		listV := &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("x"), numVal("7")}}
		mapV := &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"inner": strVal("deep")}}
		vals := func(v types.AttributeValue) map[string]types.AttributeValue {
			return map[string]types.AttributeValue{":v": v}
		}
		bounds := func(lo, hi types.AttributeValue) map[string]types.AttributeValue {
			return map[string]types.AttributeValue{":lo": lo, ":hi": hi}
		}

		runCondCases(t, c, ctx, "TypeMM", seedSemItem(), []condCase{
			// BETWEEN: attribute type != bounds type is false, not an error.
			// (Mixed-type bounds — S lo + N hi — are ValidationException, pinned
			// by TestConfBetweenMixedBoundTypes.)
			{"between: string attr, number bounds", "s BETWEEN :lo AND :hi", nil, bounds(numVal("1"), numVal("9")), false},
			{"between: number attr, string bounds", "n BETWEEN :lo AND :hi", nil, bounds(strVal("a"), strVal("z")), false},
			{"between: bool attr, number bounds", "bool BETWEEN :lo AND :hi", nil, bounds(numVal("1"), numVal("9")), false},

			// IN: mismatched operands are skipped, never an error.
			{"in: all operands mismatched", "s IN (:lo, :hi)", nil, bounds(numVal("1"), numVal("2")), false},
			{"in: skips mismatched operands", "s IN (:lo, :hi)", nil, bounds(numVal("1"), strVal("hello")), true},
			{"in: set operand matches", "ss IN (:v)", nil, vals(ssAB), true},
			{"in: bool operand matches", "bool IN (:v)", nil, vals(boolT), true},
			{"in: null operand is false", "s IN (:v)", nil, vals(nullV), false},

			// Equality is deep and set-order-insensitive; cross-type is false.
			{"set equality is order-insensitive", "ss = :v", nil, vals(ssBA), true},
			{"set subset equality is false", "ss = :v", nil, vals(ssA), false},
			{"list equality", "l = :v", nil, vals(listV), true},
			{"map equality", "m = :v", nil, vals(mapV), true},
			{"set vs scalar equality is false", "ss = :v", nil, vals(strVal("a")), false},
			{"list vs scalar equality is false", "l = :v", nil, vals(strVal("x")), false},
			{"bool vs number equality is false", "bool = :v", nil, vals(numVal("1")), false},
			{"bool vs number inequality is true", "bool <> :v", nil, vals(numVal("1")), true},
			{"map vs string inequality is true", "m <> :v", nil, vals(strVal("z")), true},

			// Ordering against a non-scalar *attribute* (scalar operand) is false.
			{"ordering: bool attr", "bool < :v", nil, vals(strVal("z")), false},
			{"ordering: set attr", "ss < :v", nil, vals(strVal("z")), false},
			{"ordering: list attr", "l <= :v", nil, vals(strVal("z")), false},
			{"ordering: map attr", "m >= :v", nil, vals(strVal("z")), false},
		})
	})
}

// Ordering comparators and BETWEEN reject a non-scalar :value operand
// (anything but S, N, B) with ValidationException — at request time, even when
// the compared attribute is missing.
func TestConfOrderingOperandTypeValidation(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "OrdOp")
		putConf(t, c, ctx, "OrdOp", seedSemItem())

		vals := func(v types.AttributeValue) map[string]types.AttributeValue {
			return map[string]types.AttributeValue{":v": v}
		}
		bounds := func(lo, hi types.AttributeValue) map[string]types.AttributeValue {
			return map[string]types.AttributeValue{":lo": lo, ":hi": hi}
		}
		boolT := &types.AttributeValueMemberBOOL{Value: true}
		nullV := &types.AttributeValueMemberNULL{Value: true}
		listV := &types.AttributeValueMemberL{Value: []types.AttributeValue{strVal("x")}}
		ssV := &types.AttributeValueMemberSS{Value: []string{"a"}}

		cases := []struct {
			name   string
			cond   string
			names  map[string]string
			values map[string]types.AttributeValue
		}{
			{"less than bool operand", "s < :v", nil, vals(boolT)},
			{"less than list operand", "s < :v", nil, vals(listV)},
			{"less than string-set operand", "s < :v", nil, vals(ssV)},
			{"less-equal null operand on null attr", "#z <= :v", map[string]string{"#z": "null"}, vals(nullV)},
			{"between null bounds", "n BETWEEN :lo AND :hi", nil, bounds(nullV, nullV)},
			{"between bool bounds", "n BETWEEN :lo AND :hi", nil, bounds(boolT, boolT)},
			{"ordering null operand, missing attr", "nope < :v", nil, vals(nullV)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
					TableName:                 aws.String("OrdOp"),
					Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
					ConditionExpression:       aws.String(tc.cond),
					ExpressionAttributeNames:  tc.names,
					ExpressionAttributeValues: tc.values,
				})
				asValidation(t, err, tc.name)
			})
		}

		// The same rejection fires on the read path (FilterExpression).
		_, err := c.Scan(ctx, &dynamodb.ScanInput{
			TableName:                 aws.String("OrdOp"),
			FilterExpression:          aws.String("s < :v"),
			ExpressionAttributeValues: vals(boolT),
		})
		asValidation(t, err, "filter with bool ordering operand")
	})
}

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

		// Real DynamoDB's documentation does not list N among size()'s supported types.
		// dynamodb-local reports ConditionalCheckFailed for both the correct digit count
		// and a wrong one, so size(N) is undefined and the comparison is simply false.
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
			// against the UNION of every expression's refs.
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

// A request that is invalid in two ways reports the failure DynamoDB
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

// A pathologically nested condition is rejected rather than crashing.
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

// Document paths that mix bare segments with #name substitutions.
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

// --- Query/Scan conformance cases ---

// seedComposite seeds n items into partition pkVal with sk 0..n-1 and a
// itemKey returns a stable string identifying a composite item (pk "|" sk) so
// tests can compare item sets across scans.
func itemKey(it map[string]types.AttributeValue) string {
	pk := it["pk"].(*types.AttributeValueMemberS)
	sk := it["sk"].(*types.AttributeValueMemberN)
	return pk.Value + "|" + sk.Value
}

// "flag" attribute ("yes" on even sk, "no" on odd). Returns the partition
// value.
func seedComposite(t *testing.T, c api, ctx context.Context, table, pkVal string, n int) {
	t.Helper()
	for i := range n {
		flag := "no"
		if i%2 == 0 {
			flag = "yes"
		}
		putConf(t, c, ctx, table, map[string]types.AttributeValue{
			"pk":   strVal(pkVal),
			"sk":   numVal(strconv.Itoa(i)),
			"flag": strVal(flag),
		})
	}
}

func TestConfQueryBasic(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 5)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.Count != 5 {
			t.Errorf("Count = %d, want 5", out.Count)
		}
	})
}

func TestConfQuerySortKeyConditions(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		cases := []struct {
			name string
			b    expression.Builder
			want int32
		}{
			{"sk<5", expression.NewBuilder().WithKeyCondition(
				expression.Key("pk").Equal(expression.Value("p1")).And(expression.Key("sk").LessThan(expression.Value(5)))), 5},
			{"sk<=4", expression.NewBuilder().WithKeyCondition(
				expression.Key("pk").Equal(expression.Value("p1")).And(expression.Key("sk").LessThanEqual(expression.Value(4)))), 5},
			{"sk>7", expression.NewBuilder().WithKeyCondition(
				expression.Key("pk").Equal(expression.Value("p1")).And(expression.Key("sk").GreaterThan(expression.Value(7)))), 2},
			{"sk>=8", expression.NewBuilder().WithKeyCondition(
				expression.Key("pk").Equal(expression.Value("p1")).And(expression.Key("sk").GreaterThanEqual(expression.Value(8)))), 2},
			{"sk BETWEEN 3 AND 6", expression.NewBuilder().WithKeyCondition(
				expression.Key("pk").Equal(expression.Value("p1")).And(expression.Key("sk").Between(expression.Value(3), expression.Value(6)))), 4},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				expr := mustExpr(t, tc.b)
				out, err := c.Query(ctx, &dynamodb.QueryInput{
					TableName:                 aws.String("ConfT"),
					KeyConditionExpression:    expr.KeyCondition(),
					ExpressionAttributeNames:  expr.Names(),
					ExpressionAttributeValues: expr.Values(),
				})
				if err != nil {
					t.Fatalf("Query: %v", err)
				}
				if out.Count != tc.want {
					t.Errorf("Count = %d, want %d", out.Count, tc.want)
				}
			})
		}
	})
}

func TestConfQueryScanIndexForward(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 5)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ScanIndexForward:          aws.Bool(false),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.Count != 5 {
			t.Fatalf("Count = %d, want 5", out.Count)
		}
		// First item should have sk=4 (highest, reverse order).
		first := out.Items[0]["sk"]
		if first != nil && first.(*types.AttributeValueMemberN).Value != "4" {
			t.Errorf("first item sk = %v, want 4", first)
		}
	})
}

func TestConfQueryPKSKOrdering(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		// pk = :v AND sk > :x
		expr1 := mustExpr(t, expression.NewBuilder().WithKeyCondition(
			expression.Key("pk").Equal(expression.Value("p1")).And(expression.Key("sk").GreaterThan(expression.Value(5)))))
		out1, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr1.KeyCondition(),
			ExpressionAttributeNames:  expr1.Names(),
			ExpressionAttributeValues: expr1.Values(),
		})
		if err != nil {
			t.Fatalf("Query pk AND sk: %v", err)
		}

		// sk > :x AND pk = :v (reversed order). The SDK expression builder refuses
		// to construct a key condition whose partition-key equality is not the
		// leftmost AND operand, so this is sent as a raw string (the shape real
		// callers hand DynamoDB). Both targets accept the reversed operand order.
		out2, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    aws.String("sk > :x AND pk = :v"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":x": numVal("5"), ":v": strVal("p1")},
		})
		if err != nil {
			t.Fatalf("Query sk AND pk: %v", err)
		}
		if out1.Count != out2.Count {
			t.Errorf("ordering: pk-first Count=%d, sk-first Count=%d, want equal", out1.Count, out2.Count)
		}
	})
}

func TestConfQueryPagination(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		// Limit=3: first page.
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(3),
		})
		if err != nil {
			t.Fatalf("Query page 1: %v", err)
		}
		if out.ScannedCount != 3 || out.Count != 3 {
			t.Errorf("page 1: Scanned=%d Count=%d, want 3/3", out.ScannedCount, out.Count)
		}
		if out.LastEvaluatedKey == nil {
			t.Fatal("page 1 LEK = nil, want non-nil")
		}

		// Resume.
		out2, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(3),
			ExclusiveStartKey:         out.LastEvaluatedKey,
		})
		if err != nil {
			t.Fatalf("Query page 2: %v", err)
		}
		if out2.Count == 0 {
			t.Error("page 2 Count = 0, want > 0")
		}
	})
}

func TestConfQueryLimitEqualsAvailable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(10),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.LastEvaluatedKey == nil {
			t.Error("LEK = nil, want non-nil (ScannedCount == Limit)")
		}

		// Resume: trailing empty page.
		out2, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(10),
			ExclusiveStartKey:         out.LastEvaluatedKey,
		})
		if err != nil {
			t.Fatalf("Query resume: %v", err)
		}
		if out2.LastEvaluatedKey != nil {
			t.Error("trailing page LEK = non-nil, want nil")
		}
	})
}

func TestConfQueryLimitZero(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(0),
		})
		asValidation(t, err, "Limit=0 should be rejected")
	})
}

func TestConfQueryFilterExpression(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		filterExpr := expression.Name("flag").Equal(expression.Value("yes"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr).WithFilter(filterExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			FilterExpression:          expr.Filter(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(3),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.ScannedCount != 3 {
			t.Errorf("ScannedCount = %d, want 3", out.ScannedCount)
		}
		if out.LastEvaluatedKey == nil {
			t.Error("LEK = nil, want non-nil (ScannedCount == Limit)")
		}
	})
}

func TestConfQueryFilterKeyAttr(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		filterExpr := expression.Name("sk").Equal(expression.Value(1))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr).WithFilter(filterExpr))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			FilterExpression:          expr.Filter(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		asValidation(t, err, "filter on key attribute should be rejected")
	})
}

func TestConfQuerySelectCount(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 5)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Select:                    types.SelectCount,
			Limit:                     aws.Int32(3),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(out.Items) > 0 {
			t.Errorf("Items = %d items, want 0 (Select=COUNT)", len(out.Items))
		}
		if out.Count != 3 {
			t.Errorf("Count = %d, want 3", out.Count)
		}
		if out.LastEvaluatedKey == nil {
			t.Error("LEK = nil, want non-nil")
		}
	})
}

func TestConfScanBasic(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 5)
		seedComposite(t, c, ctx, "ConfT", "p2", 3)

		out, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("ConfT")})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if out.Count != 8 {
			t.Errorf("Count = %d, want 8", out.Count)
		}
	})
}

func TestConfScanPagination(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		var total int32
		var start map[string]types.AttributeValue
		for {
			out, err := c.Scan(ctx, &dynamodb.ScanInput{
				TableName:         aws.String("ConfT"),
				Limit:             aws.Int32(3),
				ExclusiveStartKey: start,
			})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			total += out.Count
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
		}
		if total != 10 {
			t.Errorf("pagination total = %d, want 10", total)
		}
	})
}

// A Scan whose Limit lands exactly on the result boundary stops with
// reason "Limit reached" — LEK is set — and resuming yields an empty trailing
// page with LEK nil (the stop-reason contract, Scan side).
func TestConfScanTrailingEmptyPage(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		out, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("ConfT"), Limit: aws.Int32(10)})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if out.Count != 10 || out.ScannedCount != 10 {
			t.Errorf("Count=%d ScannedCount=%d, want 10/10", out.Count, out.ScannedCount)
		}
		if out.LastEvaluatedKey == nil {
			t.Fatal("LEK = nil, want non-nil (ScannedCount == Limit)")
		}

		out2, err := c.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String("ConfT"),
			Limit:             aws.Int32(10),
			ExclusiveStartKey: out.LastEvaluatedKey,
		})
		if err != nil {
			t.Fatalf("Scan resume: %v", err)
		}
		if out2.Count != 0 || out2.ScannedCount != 0 {
			t.Errorf("trailing page Count=%d ScannedCount=%d, want 0/0", out2.Count, out2.ScannedCount)
		}
		if out2.LastEvaluatedKey != nil {
			t.Errorf("trailing page LEK = %v, want nil", out2.LastEvaluatedKey)
		}
	})
}

// Resuming from a LEK that sits exactly on a partition boundary loses
// and repeats nothing — pages of 5, 5, then the empty terminator over two
// partitions of five items each.
func TestConfScanResumePartitionBoundary(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 5)
		seedComposite(t, c, ctx, "ConfT", "p2", 5)

		seen := map[string]int{} // "pk/sk" -> times returned
		var start map[string]types.AttributeValue
		pages := 0
		for {
			out, err := c.Scan(ctx, &dynamodb.ScanInput{
				TableName:         aws.String("ConfT"),
				Limit:             aws.Int32(5),
				ExclusiveStartKey: start,
			})
			if err != nil {
				t.Fatalf("Scan page %d: %v", pages+1, err)
			}
			pages++
			for _, it := range out.Items {
				pk := it["pk"].(*types.AttributeValueMemberS).Value
				sk := it["sk"].(*types.AttributeValueMemberN).Value
				seen[pk+"/"+sk]++
			}
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
			if pages > 4 {
				t.Fatal("pagination did not terminate after 4 pages")
			}
		}
		if pages != 3 {
			t.Errorf("pages = %d, want 3 (5, 5, empty terminator)", pages)
		}
		if len(seen) != 10 {
			t.Errorf("distinct items = %d, want 10", len(seen))
		}
		for k, n := range seen {
			if n != 1 {
				t.Errorf("item %s returned %d times, want exactly 1", k, n)
			}
		}
	})
}

// TestConfParallelScan verifies that a scan split into TotalSegments
// disjoint segments returns, in union, exactly the full item set, with every
// item appearing in precisely one segment (no overlap, no omission).
func TestConfParallelScan(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		// Seed several partitions so each segment is non-trivial.
		const itemsPerPartition, partitions = 10, 3
		for pi := range partitions {
			seedComposite(t, c, ctx, "ConfT", fmt.Sprintf("p%d", pi), itemsPerPartition)
		}
		totalItems := itemsPerPartition * partitions

		// Reference: a full scan gives the complete item set.
		full, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("ConfT")})
		if err != nil {
			t.Fatalf("full Scan: %v", err)
		}
		fullSet := map[string]bool{}
		for _, it := range full.Items {
			fullSet[itemKey(it)] = true
		}
		if len(fullSet) != totalItems {
			t.Fatalf("full scan returned %d distinct items, want %d", len(fullSet), totalItems)
		}

		// Parallel scan: TotalSegments=3, one scan per Segment 0..2.
		const totalSegments = 3
		union := map[string]bool{}
		var unionCount int
		seenInSegments := map[string]int{}
		for seg := range totalSegments {
			out, err := c.Scan(ctx, &dynamodb.ScanInput{
				TableName:     aws.String("ConfT"),
				TotalSegments: aws.Int32(totalSegments),
				Segment:       aws.Int32(int32(seg)),
			})
			if err != nil {
				t.Fatalf("Scan segment %d: %v", seg, err)
			}
			unionCount += len(out.Items)
			for _, it := range out.Items {
				k := itemKey(it)
				union[k] = true
				seenInSegments[k]++
			}
		}

		// Union equals the full item set (all items, no omission).
		if len(union) != totalItems {
			t.Errorf("parallel scan union has %d distinct items, want %d (== full scan)", len(union), totalItems)
		}
		for k := range fullSet {
			if !union[k] {
				t.Errorf("item %q missing from parallel scan union", k)
			}
		}
		// No overlap: every item appears in exactly one segment.
		if unionCount != totalItems {
			t.Errorf("parallel scan returned %d items total, want %d (no duplicates)", unionCount, totalItems)
		}
		for k, n := range seenInSegments {
			if n != 1 {
				t.Errorf("item %q appeared in %d segments, want 1", k, n)
			}
		}
	})
}

func TestConfScanLimitZero(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		_, err := c.Scan(ctx, &dynamodb.ScanInput{
			TableName: aws.String("ConfT"),
			Limit:     aws.Int32(0),
		})
		asValidation(t, err, "Scan Limit=0 should be rejected")
	})
}

func TestConfBeginsWithOnNumber(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(
			expression.Key("pk").Equal(expression.Value("p1")).And(
				expression.Key("sk").BeginsWith("1"))))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		asValidation(t, err, "begins_with on N sort key should be rejected")
	})
}

// TestConfQueryBeginsWithOnStringSortKey covers the begins_with-on-S gap:
// TestConfQuerySortKeyConditions only exercised =,<,<=,>,>=,BETWEEN
// on an N sort key. It creates a composite table with an S sort key and
// asserts begins_with(sk, :prefix) returns exactly the prefix-matching items.
// This is the case that would have caught the bug (the S-key upper-bound
// successor bound as BLOB instead of TEXT).
func TestConfQueryBeginsWithOnStringSortKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("ConfT"),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
			},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
			},
			BillingMode: types.BillingModePayPerRequest,
		})
		if err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		for _, sk := range []string{"apple", "apricot", "avocado", "banana", "cherry"} {
			putConf(t, c, ctx, "ConfT", map[string]types.AttributeValue{
				"pk": strVal("p1"),
				"sk": strVal(sk),
			})
		}

		// begins_with(sk, "ap") matches apple, apricot (avocado starts with "av"
		// and must be excluded; banana and cherry don't match either).
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(
			expression.Key("pk").Equal(expression.Value("p1")).And(
				expression.Key("sk").BeginsWith("ap"))))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.Count != 2 {
			t.Errorf("Count = %d, want 2 (only prefix-matching items)", out.Count)
		}

		// begins_with on a prefix with no matches returns 0.
		expr2 := mustExpr(t, expression.NewBuilder().WithKeyCondition(
			expression.Key("pk").Equal(expression.Value("p1")).And(
				expression.Key("sk").BeginsWith("zz"))))
		out2, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr2.KeyCondition(),
			ExpressionAttributeNames:  expr2.Names(),
			ExpressionAttributeValues: expr2.Values(),
		})
		if err != nil {
			t.Fatalf("Query (no-match): %v", err)
		}
		if out2.Count != 0 {
			t.Errorf("Count = %d, want 0 for non-matching prefix", out2.Count)
		}
	})
}

func TestConfKeyConditionRejections(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		cases := []struct {
			name string
			src  string
		}{
			{"OR", "pk = :v OR sk = :s"},
			{"NOT", "NOT pk = :v"},
			{"non-equality PK", "pk > :v"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := c.Query(ctx, &dynamodb.QueryInput{
					TableName:                 aws.String("ConfT"),
					KeyConditionExpression:    aws.String(tc.src),
					ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("p1"), ":s": numVal("1")},
				})
				asValidation(t, err, tc.name+" should be rejected")
			})
		}
	})
}

func TestConfExclusiveStartKeyValidation(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		// Partition mismatch.
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ExclusiveStartKey: map[string]types.AttributeValue{
				"pk": strVal("WRONG"),
				"sk": numVal("0"),
			},
		})
		asValidation(t, err, "partition mismatch should be rejected")

		// Key carrying extra attributes.
		_, err = c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ExclusiveStartKey: map[string]types.AttributeValue{
				"pk":    strVal("p1"),
				"sk":    numVal("0"),
				"extra": strVal("x"),
			},
		})
		asValidation(t, err, "extra attributes on ExclusiveStartKey should be rejected")
	})
}

func TestConfIndexName(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G2"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query with IndexName: %v", err)
		}
		if len(out.Items) != 1 {
			t.Fatalf("got %d items, want 1", len(out.Items))
		}
		if v, ok := out.Items[0]["pk"].(*types.AttributeValueMemberS); !ok || v.Value != "E" {
			t.Errorf("item pk = %v, want E", out.Items[0]["pk"])
		}
	})
}

func TestConfLegacyParams(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		_, isAdapter := c.(*ddbsqlite.Adapter)
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName: aws.String("ConfT"),
			KeyConditions: map[string]types.Condition{
				"pk": {ComparisonOperator: types.ComparisonOperatorEq, AttributeValueList: []types.AttributeValue{strVal("p1")}},
			},
		})
		if isAdapter {
			// The adapter deliberately does not implement the deprecated
			// pre-expression parameters; it rejects a non-empty KeyConditions
			// with a ValidationException so a caller never believes the
			// constraint was applied.
			asValidation(t, err, "legacy KeyConditions should be rejected on the adapter")
			return
		}
		// The reference (dynamodb-local 3.3.1) still accepts the deprecated
		// pre-expression KeyConditions parameter (returns the items).
		if err != nil {
			t.Fatalf("reference accepted legacy KeyConditions: %v", err)
		}
	})
}

func TestConfPresentButEmptyExpressions(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String("ConfT"),
			KeyConditionExpression: aws.String(""),
		})
		asValidation(t, err, "empty KeyConditionExpression should be rejected")
	})
}

func TestConfQueryLimitExceedsAvailable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(15),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.ScannedCount != 10 {
			t.Errorf("ScannedCount = %d, want 10", out.ScannedCount)
		}
		if out.LastEvaluatedKey != nil {
			t.Error("LEK = non-nil, want nil (exhausted)")
		}
	})
}

func TestConfQueryLimitAfterKeyNarrowing(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1")).And(
			expression.Key("sk").Between(expression.Value(2), expression.Value(7)))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Limit:                     aws.Int32(3),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.ScannedCount != 3 {
			t.Errorf("ScannedCount = %d, want 3", out.ScannedCount)
		}
		if out.LastEvaluatedKey == nil {
			t.Error("LEK = nil, want non-nil")
		}
	})
}

func TestConfQueryReverseWithLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 5)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ScanIndexForward:          aws.Bool(false),
			Limit:                     aws.Int32(1),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.Count != 1 {
			t.Fatalf("Count = %d, want 1", out.Count)
		}
		sk := out.Items[0]["sk"].(*types.AttributeValueMemberN).Value
		if sk != "4" {
			t.Errorf("first reverse sk = %s, want 4", sk)
		}
	})
}

func TestConfSelectCountWithLimitAndFilter(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 10)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		filterExpr := expression.Name("flag").Equal(expression.Value("yes"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr).WithFilter(filterExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			FilterExpression:          expr.Filter(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Select:                    types.SelectCount,
			Limit:                     aws.Int32(10),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(out.Items) > 0 {
			t.Errorf("Items = %d, want 0", len(out.Items))
		}
		if out.ScannedCount != 10 {
			t.Errorf("ScannedCount = %d, want 10", out.ScannedCount)
		}
		if out.LastEvaluatedKey == nil {
			t.Error("LEK = nil, want non-nil (ScannedCount == Limit)")
		}
	})
}

// --- GSI conformance cases ---

// itemSet returns the set of "pk" values in items, for comparing GSI results
// whose tied sort keys have no guaranteed relative order.
func itemSet(items []map[string]types.AttributeValue) map[string]bool {
	set := map[string]bool{}
	for _, it := range items {
		if v, ok := it["pk"].(*types.AttributeValueMemberS); ok {
			set[v.Value] = true
		}
	}
	return set
}

// wantSet asserts exactly the pks in want (order-insensitive) are present.
func wantSet(t *testing.T, items []map[string]types.AttributeValue, want ...string) {
	t.Helper()
	got := itemSet(items)
	if len(got) != len(want) {
		t.Fatalf("got %d items %v, want %v", len(got), pks(items), want)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing pk %q in %v", w, got)
		}
	}
}

func sv(s string) types.AttributeValue { return &types.AttributeValueMemberS{Value: s} }

// pks returns the sorted "pk" values from a list of items.
func pks(items []map[string]types.AttributeValue) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if v, ok := it["pk"].(*types.AttributeValueMemberS); ok {
			out = append(out, v.Value)
		}
	}
	sort.Strings(out)
	return out
}

// wantAttrNames asserts item carries exactly the sorted attribute names want.
func wantAttrNames(t *testing.T, item map[string]types.AttributeValue, want []string, msg string) {
	t.Helper()
	got := itemAttrNamesConf(item)
	if len(got) != len(want) {
		t.Errorf("%s: attrs = %v, want %v", msg, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: attrs = %v, want %v", msg, got, want)
			return
		}
	}
}

// Basic GSI Query — IndexName + gsi_pk = :v returns the right items in
// GSI sort order. Tied sort keys (A,C share gsi_sk=s1) have unspecified order,
// so items are compared as a set.
func TestConfGSIBasicQuery(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "A", "B", "C")
	})
}

// Sparse GSI — D has no GSI attributes, so it is absent from both a
// Query on gsi-all and a Scan of gsi-all.
func TestConfGSISparse(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		// Query gsi_pk=G2: only E is indexed there.
		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G2"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "E")

		// Scan gsi-all: D (no GSI attrs) must be absent.
		scan, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("ConfT"), IndexName: aws.String("gsi-all")})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if set := itemSet(scan.Items); set["D"] {
			t.Errorf("Scan gsi-all returned D; sparse item must be absent")
		}
		wantSet(t, scan.Items, "A", "B", "C", "E")
	})
}

// Non-unique GSI key — A and C share gsi_sk=s1 under gsi_pk=G1 and
// both must be returned.
func TestConfGSINonUnique(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").Equal(expression.Value("s1")))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "A", "C")
	})
}

// GSI sort-key conditions — =, <, <=, >, >=, BETWEEN and begins_with
// on the S gsi_sk sort key of gsi-all for partition G1.
func TestConfGSISortKeyConditions(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		cases := []struct {
			name string
			b    expression.Builder
			want []string
		}{
			{"gsi_sk = s1", expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").Equal(expression.Value("s1")))), []string{"A", "C"}},
			{"gsi_sk < s2", expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").LessThan(expression.Value("s2")))), []string{"A", "C"}},
			{"gsi_sk <= s1", expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").LessThanEqual(expression.Value("s1")))), []string{"A", "C"}},
			{"gsi_sk > s1", expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").GreaterThan(expression.Value("s1")))), []string{"B"}},
			{"gsi_sk >= s2", expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").GreaterThanEqual(expression.Value("s2")))), []string{"B"}},
			{"gsi_sk BETWEEN s1 AND s2", expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").Between(expression.Value("s1"), expression.Value("s2")))), []string{"A", "B", "C"}},
			{"begins_with(gsi_sk, s)", expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").BeginsWith("s"))), []string{"A", "B", "C"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				expr := mustExpr(t, tc.b)
				out, err := c.Query(ctx, &dynamodb.QueryInput{
					TableName:                 aws.String("ConfT"),
					IndexName:                 aws.String("gsi-all"),
					KeyConditionExpression:    expr.KeyCondition(),
					ExpressionAttributeNames:  expr.Names(),
					ExpressionAttributeValues: expr.Values(),
				})
				if err != nil {
					t.Fatalf("Query: %v", err)
				}
				wantSet(t, out.Items, tc.want...)
			})
		}
	})
}

// ScanIndexForward=false on a GSI returns items in descending GSI sort
// order — B (gsi_sk=s2) first, then the s1 tie (A,C).
func TestConfGSIScanIndexForward(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ScanIndexForward:          aws.Bool(false),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if v, ok := out.Items[0]["pk"].(*types.AttributeValueMemberS); !ok || v.Value != "B" {
			t.Errorf("first item pk = %v, want B (highest gsi_sk in DESC order)", out.Items[0]["pk"])
		}
		wantSet(t, out.Items, "A", "B", "C")
	})
}

// GSI pagination — Limit=2 with resume to exhaustion.
func TestConfGSIPagination(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		var total int
		var start map[string]types.AttributeValue
		for {
			out, err := c.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String("ConfT"),
				IndexName:                 aws.String("gsi-all"),
				KeyConditionExpression:    expr.KeyCondition(),
				ExpressionAttributeNames:  expr.Names(),
				ExpressionAttributeValues: expr.Values(),
				Limit:                     aws.Int32(2),
				ExclusiveStartKey:         start,
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			total += len(out.Items)
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
		}
		if total != 3 {
			t.Errorf("pagination total = %d, want 3", total)
		}
	})
}

// ConsistentRead=true is rejected on a GSI Query and Scan.
func TestConfGSIConsistentRead(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ConsistentRead:            aws.Bool(true),
		})
		asValidation(t, err, "ConsistentRead=true on GSI Query")

		_, err = c.Scan(ctx, &dynamodb.ScanInput{
			TableName:      aws.String("ConfT"),
			IndexName:      aws.String("gsi-all"),
			ConsistentRead: aws.Bool(true),
		})
		asValidation(t, err, "ConsistentRead=true on GSI Scan")
	})
}

// KEYS_ONLY projection — a query on gsi-keys returns only the table
// primary key plus the index key ({gsi_pk, pk, sk}).
func TestConfGSIProjectionKeysOnly(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-keys"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "A", "B", "C")
		for _, it := range out.Items {
			wantAttrNames(t, it, []string{"gsi_pk", "pk", "sk"}, "KEYS_ONLY item")
		}
	})
}

// INCLUDE projection — gsi-incl returns table keys + GSI keys + the
// included non-key attrs, omitting absent ones (C has no proj1/proj2).
func TestConfGSIProjectionInclude(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").Equal(expression.Value("s1")))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-incl"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(out.Items) != 2 {
			t.Fatalf("got %d items, want 2", len(out.Items))
		}
		for _, it := range out.Items {
			pk := it["pk"].(*types.AttributeValueMemberS).Value
			switch pk {
			case "A":
				wantAttrNames(t, it, []string{"gsi_pk", "gsi_sk", "pk", "proj1", "proj2", "sk"}, "INCLUDE item A")
			case "C":
				wantAttrNames(t, it, []string{"gsi_pk", "gsi_sk", "pk", "sk"}, "INCLUDE item C")
			default:
				t.Errorf("unexpected item pk %q", pk)
			}
		}
	})
}

// Nested INCLUDE: a NonKeyAttributes entry that is
// a document path ("obj.a") is accepted at CreateTable but never projected —
// the read-time trim keeps top-level names only, so the item's "obj"
// attribute is absent from GSI reads. dynamodb-local 3.3.1 behaves
// identically; this case pins the match dual-target.
func TestConfGSINestedInclude(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("NestIncl"),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
			},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gk"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("gsi-nest"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeInclude, NonKeyAttributes: []string{"obj.a"}},
			}},
			BillingMode: types.BillingModePayPerRequest,
		})
		if err != nil {
			t.Fatalf("CreateTable with nested INCLUDE entry: %v", err)
		}
		putConf(t, c, ctx, "NestIncl", map[string]types.AttributeValue{
			"pk":  sv("P1"),
			"sk":  sv("S1"),
			"gk":  sv("G1"),
			"obj": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"a": sv("aval")}},
		})

		keyExpr := expression.Key("gk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("NestIncl"),
			IndexName:                 aws.String("gsi-nest"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(out.Items) != 1 {
			t.Fatalf("got %d items, want 1", len(out.Items))
		}
		wantAttrNames(t, out.Items[0], []string{"gk", "pk", "sk"}, "nested INCLUDE item")
	})
}

// ALL projection — a query on gsi-all returns every attribute.
func TestConfGSIProjectionAll(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "A", "B", "C")
		// A carries the full attribute set (including non-projected "extra"),
		// proving the ALL projection returns every attribute.
		for _, it := range out.Items {
			if pk, ok := it["pk"].(*types.AttributeValueMemberS); ok && pk.Value == "A" {
				wantAttrNames(t, it, []string{"extra", "gsi_pk", "gsi_sk", "pk", "proj1", "proj2", "sk"}, "ALL projection item A")
			}
		}
	})
}

// Select=ALL_PROJECTED_ATTRIBUTES on a GSI returns the projected attrs.
func TestConfGSISelectAllProjectedAttributes(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").Equal(expression.Value("s1")))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-incl"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Select:                    types.SelectAllProjectedAttributes,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		for _, it := range out.Items {
			if pk, ok := it["pk"].(*types.AttributeValueMemberS); ok && pk.Value == "A" {
				wantAttrNames(t, it, []string{"gsi_pk", "gsi_sk", "pk", "proj1", "proj2", "sk"}, "ALL_PROJECTED_ATTRIBUTES item A")
			}
		}
	})
}

// Select=ALL_ATTRIBUTES on a non-ALL GSI is a ValidationException.
func TestConfGSISelectAllAttributes(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-incl"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Select:                    types.SelectAllAttributes,
		})
		asValidation(t, err, "Select=ALL_ATTRIBUTES on non-ALL GSI")
	})
}

// A non-GSI key attribute in KeyConditionExpression is a
// ValidationException.
func TestConfGSINonGsiAttr(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		// pk is a table key but not a gsi-all key; conditioning on it is invalid.
		keyExpr := expression.Key("pk").Equal(expression.Value("A"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		asValidation(t, err, "non-GSI attr in GSI KeyCondition")
	})
}

// GSI Scan — gsi-all returns every indexed item (D excluded) with a
// nil LEK.
func TestConfGSIScan(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		out, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("ConfT"), IndexName: aws.String("gsi-all")})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if set := itemSet(out.Items); set["D"] {
			t.Errorf("Scan gsi-all returned D; sparse item must be absent")
		}
		wantSet(t, out.Items, "A", "B", "C", "E")
		if out.LastEvaluatedKey != nil {
			t.Errorf("LEK = %v, want nil for full scan", out.LastEvaluatedKey)
		}
	})
}

// GSI Scan pagination — Limit=2 with resume to exhaustion.
func TestConfGSIScanPagination(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		var total int
		var start map[string]types.AttributeValue
		for {
			out, err := c.Scan(ctx, &dynamodb.ScanInput{
				TableName:         aws.String("ConfT"),
				IndexName:         aws.String("gsi-all"),
				Limit:             aws.Int32(2),
				ExclusiveStartKey: start,
			})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			total += len(out.Items)
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
		}
		if total != 4 {
			t.Errorf("pagination total = %d, want 4", total)
		}
	})
}

// begins_with on the GSI sort key performs a prefix match.
func TestConfGSIBeginsWith(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		// begins_with(gsi_sk, "s1") matches only gsi_sk values that start with
		// "s1" (A, C) — narrower than "s" (all), proving prefix semantics.
		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1")).And(expression.Key("gsi_sk").BeginsWith("s1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "A", "C")
	})
}

// UpdateItem changes a GSI key — the item moves to the new GSI
// partition (old partition no longer returns it, new one does).
func TestConfGSIUpdateChangesKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		upd := expression.Set(expression.Name("gsi_pk"), expression.Value("G3"))
		uexpr := mustExpr(t, expression.NewBuilder().WithUpdate(upd))
		if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String("ConfT"),
			Key:                       map[string]types.AttributeValue{"pk": sv("A"), "sk": sv("a")},
			UpdateExpression:          uexpr.Update(),
			ExpressionAttributeNames:  uexpr.Names(),
			ExpressionAttributeValues: uexpr.Values(),
		}); err != nil {
			t.Fatalf("UpdateItem change gsi key: %v", err)
		}

		query := func(partition string) []map[string]types.AttributeValue {
			keyExpr := expression.Key("gsi_pk").Equal(expression.Value(partition))
			expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
			out, err := c.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String("ConfT"),
				IndexName:                 aws.String("gsi-all"),
				KeyConditionExpression:    expr.KeyCondition(),
				ExpressionAttributeNames:  expr.Names(),
				ExpressionAttributeValues: expr.Values(),
			})
			if err != nil {
				t.Fatalf("Query %s: %v", partition, err)
			}
			return out.Items
		}

		wantSet(t, query("G1"), "B", "C") // A moved away from G1
		wantSet(t, query("G3"), "A")      // A now lives in G3
	})
}

// UpdateItem removes a GSI key — the item becomes sparse (absent from
// the GSI Query).
func TestConfGSIUpdateRemovesKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		urexpr := mustExpr(t, expression.NewBuilder().WithUpdate(expression.Remove(expression.Name("gsi_pk"))))
		if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                aws.String("ConfT"),
			Key:                      map[string]types.AttributeValue{"pk": sv("B"), "sk": sv("b")},
			UpdateExpression:         urexpr.Update(),
			ExpressionAttributeNames: urexpr.Names(),
		}); err != nil {
			t.Fatalf("UpdateItem remove gsi key: %v", err)
		}

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "A", "C") // B is now sparse
	})
}

// Sparse GSI items are ordinary items — updatable while absent from
// the index, moved into the index by setting the GSI key attributes, and back
// out by removing the composite sort key.
func TestConfGSISparseUpdate(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")

		// Sparse item: no GSI key attributes.
		putConf(t, c, ctx, "ConfT", map[string]types.AttributeValue{"pk": sv("D"), "sk": sv("d")})

		update := func(u expression.UpdateBuilder) {
			t.Helper()
			uexpr := mustExpr(t, expression.NewBuilder().WithUpdate(u))
			if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
				TableName:                 aws.String("ConfT"),
				Key:                       map[string]types.AttributeValue{"pk": sv("D"), "sk": sv("d")},
				UpdateExpression:          uexpr.Update(),
				ExpressionAttributeNames:  uexpr.Names(),
				ExpressionAttributeValues: uexpr.Values(),
			}); err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
		}
		queryG9 := func() []map[string]types.AttributeValue {
			t.Helper()
			kexpr := mustExpr(t, expression.NewBuilder().WithKeyCondition(
				expression.Key("gsi_pk").Equal(expression.Value("G9"))))
			out, err := c.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String("ConfT"),
				IndexName:                 aws.String("gsi-all"),
				KeyConditionExpression:    kexpr.KeyCondition(),
				ExpressionAttributeNames:  kexpr.Names(),
				ExpressionAttributeValues: kexpr.Values(),
			})
			if err != nil {
				t.Fatalf("Query gsi-all: %v", err)
			}
			return out.Items
		}

		// A non-key update succeeds and keeps the item out of the index.
		update(expression.Set(expression.Name("proj1"), expression.Value("p")))
		scan, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("ConfT"), IndexName: aws.String("gsi-all")})
		if err != nil {
			t.Fatalf("Scan gsi-all: %v", err)
		}
		if itemSet(scan.Items)["D"] {
			t.Error("sparse item D present in gsi-all after non-key update")
		}

		// Setting both GSI key attributes moves the item into the index.
		update(expression.Set(expression.Name("gsi_pk"), expression.Value("G9")).
			Set(expression.Name("gsi_sk"), expression.Value("s9")))
		wantSet(t, queryG9(), "D")

		// Removing the composite GSI's sort key takes the item back out.
		update(expression.Remove(expression.Name("gsi_sk")))
		if items := queryG9(); len(items) != 0 {
			t.Errorf("after REMOVE gsi_sk, gsi_pk=G9 returned %v, want empty", itemSet(items))
		}
		// The item itself is intact.
		got, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String("ConfT"),
			Key:       map[string]types.AttributeValue{"pk": sv("D"), "sk": sv("d")},
		})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got.Item == nil {
			t.Error("GetItem D = nil, want present (only the index entry is gone)")
		}
	})
}

// DeleteItem removes the item from the GSI.
func TestConfGSIDeleteRemoves(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		if _, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String("ConfT"),
			Key:       map[string]types.AttributeValue{"pk": sv("C"), "sk": sv("c")},
		}); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantSet(t, out.Items, "A", "B") // C removed from GSI
	})
}

// A partition-only GSI — gsi_pk = :v returns every item in that
// partition.
func TestConfGSIPartitionOnly(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-keys"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query gsi-keys: %v", err)
		}
		wantSet(t, out.Items, "A", "B", "C")
	})
}

// GSI partition key equal to the table partition key (overlapping key)
// is valid; the GSI queries correctly. Uses its own table "OvT".
func TestConfGSIOverlappingKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("OvT"),
			KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("pk-index"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			}},
			BillingMode: types.BillingModePayPerRequest,
		})
		if err != nil {
			t.Fatalf("CreateTable overlapping key: %v", err)
		}
		putConf(t, c, ctx, "OvT", map[string]types.AttributeValue{"pk": sv("A")})
		keyExpr := expression.Key("pk").Equal(expression.Value("A"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("OvT"),
			IndexName:                 aws.String("pk-index"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(out.Items) != 1 {
			t.Errorf("got %d, want 1", len(out.Items))
		}
	})
}

// an ExclusiveStartKey whose GSI partition does not match the
// KeyConditionExpression partition is a ValidationException.
func TestConfGSIEskPartitionMismatch(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		// Correct union shape (pk, sk, gsi_pk, gsi_sk) but gsi_pk=G2 mismatches
		// the key condition's G1 partition.
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ExclusiveStartKey: map[string]types.AttributeValue{
				"pk": sv("A"), "sk": sv("a"), "gsi_pk": sv("G2"), "gsi_sk": sv("s1"),
			},
		})
		asValidation(t, err, "ESK GSI partition mismatch")
	})
}

// An item with gsi_pk but no gsi_sk (composite GSI) is accepted but
// absent from the GSI Query and Scan.
func TestConfGSICompositeSortAbsent(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		// F carries gsi_pk but no gsi_sk — write accepted, not indexed by gsi-all.
		putConf(t, c, ctx, "ConfT", map[string]types.AttributeValue{
			"pk": sv("F"), "sk": sv("f"), "gsi_pk": sv("G5"),
		})

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G5"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(out.Items) != 0 {
			t.Errorf("Query gsi_pk=G5 returned %v, want none (sort key absent)", itemSet(out.Items))
		}

		scan, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("ConfT"), IndexName: aws.String("gsi-all")})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if set := itemSet(scan.Items); set["F"] {
			t.Errorf("Scan gsi-all returned F; composite sort-absent item must be absent")
		}
	})
}

// GSI key type mismatch on PutItem — gsi_pk as Number — is a
// ValidationException and atomic (GetItem finds nothing).
func TestConfGSIKeyTypeMismatchPut(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")

		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("ConfT"), Item: map[string]types.AttributeValue{
			"pk": sv("X"), "sk": sv("x"), "gsi_pk": numVal("1"), "gsi_sk": sv("s1"),
		}})
		asValidation(t, err, "put gsi_pk as Number")

		// Atomic: the rejected write left no item behind.
		got, gerr := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("ConfT"), Key: map[string]types.AttributeValue{"pk": sv("X"), "sk": sv("x")}})
		if gerr != nil {
			t.Fatalf("GetItem: %v", gerr)
		}
		if got.Item != nil {
			t.Errorf("after rejected put, GetItem = %v, want nil (atomic)", got.Item)
		}
	})
}

// A non-scalar GSI key attribute (L/BOOL/SS/NULL) on PutItem is a
// ValidationException.
func TestConfGSINonScalarKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")

		cases := []struct {
			name  string
			gsiPK types.AttributeValue
		}{
			{"L", &types.AttributeValueMemberL{Value: []types.AttributeValue{sv("a")}}},
			{"BOOL", &types.AttributeValueMemberBOOL{Value: true}},
			{"SS", &types.AttributeValueMemberSS{Value: []string{"a"}}},
			{"NULL", &types.AttributeValueMemberNULL{Value: true}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("ConfT"), Item: map[string]types.AttributeValue{
					"pk": sv("X"), "sk": sv("x" + tc.name), "gsi_pk": tc.gsiPK, "gsi_sk": sv("s1"),
				}})
				asValidation(t, err, "non-scalar gsi key "+tc.name)
			})
		}
	})
}

// GSI key type mismatch on UpdateItem (SET gsi_pk = :n) is a
// ValidationException and atomic (item unchanged).
func TestConfGSIKeyTypeMismatchUpdate(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		uexpr := mustExpr(t, expression.NewBuilder().WithUpdate(
			expression.Set(expression.Name("gsi_pk"), expression.Value(numVal("9")))))
		_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String("ConfT"),
			Key:                       map[string]types.AttributeValue{"pk": sv("A"), "sk": sv("a")},
			UpdateExpression:          uexpr.Update(),
			ExpressionAttributeNames:  uexpr.Names(),
			ExpressionAttributeValues: uexpr.Values(),
		})
		asValidation(t, err, "update gsi_pk as Number")

		// Atomic: item A unchanged, still in G1.
		got, gerr := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("ConfT"), Key: map[string]types.AttributeValue{"pk": sv("A"), "sk": sv("a")}})
		if gerr != nil {
			t.Fatalf("GetItem: %v", gerr)
		}
		if got.Item == nil {
			t.Fatalf("GetItem A = nil, want present (atomic)")
		}
		if v, ok := got.Item["gsi_pk"].(*types.AttributeValueMemberS); !ok || v.Value != "G1" {
			t.Errorf("gsi_pk = %v, want G1 (unchanged)", got.Item["gsi_pk"])
		}
	})
}

// An empty string as a GSI partition key value is a
// ValidationException.
func TestConfGSIEmptyKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")

		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("ConfT"), Item: map[string]types.AttributeValue{
			"pk": sv("X"), "sk": sv("x"), "gsi_pk": sv(""), "gsi_sk": sv("s1"),
		}})
		asValidation(t, err, "empty gsi_pk value")
	})
}

// GSI ExclusiveStartKey shape validation — table-only, GSI-only, and
// union-plus-extra shapes are rejected; the exact union is accepted.
func TestConfGSIEskShape(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		seedGsiConformance(t, c, ctx, "ConfT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		bad := []struct {
			name string
			esk  map[string]types.AttributeValue
		}{
			{"table-only", map[string]types.AttributeValue{"pk": sv("A"), "sk": sv("a")}},
			{"gsi-only", map[string]types.AttributeValue{"gsi_pk": sv("G1"), "gsi_sk": sv("s1")}},
			{"union-plus-extra", map[string]types.AttributeValue{
				"pk": sv("A"), "sk": sv("a"), "gsi_pk": sv("G1"), "gsi_sk": sv("s1"), "extra": sv("x"),
			}},
		}
		for _, tc := range bad {
			t.Run(tc.name, func(t *testing.T) {
				_, err := c.Query(ctx, &dynamodb.QueryInput{
					TableName:                 aws.String("ConfT"),
					IndexName:                 aws.String("gsi-all"),
					KeyConditionExpression:    expr.KeyCondition(),
					ExpressionAttributeNames:  expr.Names(),
					ExpressionAttributeValues: expr.Values(),
					ExclusiveStartKey:         tc.esk,
				})
				asValidation(t, err, "ESK shape "+tc.name)
			})
		}

		// Exact union (table keys + GSI keys) is accepted.
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ExclusiveStartKey: map[string]types.AttributeValue{
				"pk": sv("A"), "sk": sv("a"), "gsi_pk": sv("G1"), "gsi_sk": sv("s1"),
			},
		})
		if err != nil {
			t.Fatalf("ESK exact union rejected: %v", err)
		}
	})
}

// A duplicate AttributeDefinition is a ValidationException, with and
// without GSIs.
func TestConfGSIDuplicateAttrDef(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()

		// Without GSIs: duplicate pk AttributeDefinition.
		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("Dup1"),
			KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			},
			BillingMode: types.BillingModePayPerRequest,
		})
		asValidation(t, err, "duplicate attr def without GSI")

		// With GSIs: duplicate gsi_pk AttributeDefinition.
		_, err = c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("Dup2"),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
			},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gsi_pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gsi_pk"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("g"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gsi_pk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			}},
			BillingMode: types.BillingModePayPerRequest,
		})
		asValidation(t, err, "duplicate attr def with GSI")
	})
}

// GSI IndexName validation — illegal characters and names too short
// are ValidationExceptions.
func TestConfGSIIndexNameValidation(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()

		build := func(indexName string) *dynamodb.CreateTableInput {
			return &dynamodb.CreateTableInput{
				TableName: aws.String("Idx"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
				},
				AttributeDefinitions: []types.AttributeDefinition{
					{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
					{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
					{AttributeName: aws.String("gsi_pk"), AttributeType: types.ScalarAttributeTypeS},
				},
				GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
					IndexName:  aws.String(indexName),
					KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gsi_pk"), KeyType: types.KeyTypeHash}},
					Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
				}},
				BillingMode: types.BillingModePayPerRequest,
			}
		}

		// Illegal characters (space, exclamation).
		_, err := c.CreateTable(ctx, build("bad name!"))
		asValidation(t, err, "illegal index name chars")

		// Too short (2 chars; minimum is 3).
		_, err = c.CreateTable(ctx, build("ab"))
		asValidation(t, err, "index name too short")
	})
}

// DescribeTable returns the GSI defs — key schemas round-trip.
func TestConfGSIDescribeTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")

		out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("ConfT")})
		if err != nil {
			t.Fatalf("DescribeTable: %v", err)
		}
		got := map[string]types.GlobalSecondaryIndexDescription{}
		for _, g := range out.Table.GlobalSecondaryIndexes {
			got[*g.IndexName] = g
		}
		want := []string{"gsi-all", "gsi-keys", "gsi-incl"}
		for _, w := range want {
			g, ok := got[w]
			if !ok {
				t.Errorf("missing GSI %q in %v", w, got)
				continue
			}
			// Key schema: one HASH, maybe one RANGE.
			if len(g.KeySchema) < 1 {
				t.Errorf("GSI %q: empty key schema", w)
			}
		}
	})
}

// orderPks returns the "pk" values of items in returned order (not sorted).
func orderPks(items []map[string]types.AttributeValue) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if v, ok := it["pk"].(*types.AttributeValueMemberS); ok {
			out = append(out, v.Value)
		}
	}
	return out
}

// TestConfGSIDescPagination: GSI Query with ScanIndexForward=false and Limit
// paginates in descending GSI sort order across a page boundary — resuming via
// LastEvaluatedKey must continue in DESC order with no skip or duplicate.
func TestConfGSIDescPagination(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		// Four G1 items with distinct gsi_sk so DESC order is deterministic.
		// gsi_sk s1..s4 -> pk P1..P4; full DESC order: P4,P3,P2,P1.
		for _, it := range []map[string]types.AttributeValue{
			{"pk": sv("P1"), "sk": sv("p1"), "gsi_pk": sv("G1"), "gsi_sk": sv("s1")},
			{"pk": sv("P2"), "sk": sv("p2"), "gsi_pk": sv("G1"), "gsi_sk": sv("s2")},
			{"pk": sv("P3"), "sk": sv("p3"), "gsi_pk": sv("G1"), "gsi_sk": sv("s3")},
			{"pk": sv("P4"), "sk": sv("p4"), "gsi_pk": sv("G1"), "gsi_sk": sv("s4")},
		} {
			putConf(t, c, ctx, "ConfT", it)
		}

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		query := func(start map[string]types.AttributeValue) (*dynamodb.QueryOutput, error) {
			return c.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String("ConfT"),
				IndexName:                 aws.String("gsi-all"),
				KeyConditionExpression:    expr.KeyCondition(),
				ExpressionAttributeNames:  expr.Names(),
				ExpressionAttributeValues: expr.Values(),
				ScanIndexForward:          aws.Bool(false),
				Limit:                     aws.Int32(2),
				ExclusiveStartKey:         start,
			})
		}

		// Accumulate pages by resuming via LastEvaluatedKey; the final page may
		// or may not clear LastEvaluatedKey depending on target, so loop to
		// exhaustion and assert the FULL descending order (no skip/dup).
		var ordered []string
		var start map[string]types.AttributeValue
		pages := 0
		for {
			out, err := query(start)
			if err != nil {
				t.Fatalf("Query page %d: %v", pages+1, err)
			}
			pages++
			ordered = append(ordered, orderPks(out.Items)...)
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
		}
		if pages < 2 {
			t.Fatalf("DESC pagination produced %d page(s), want >= 2 across a boundary", pages)
		}
		if !equalStrings(ordered, []string{"P4", "P3", "P2", "P1"}) {
			t.Errorf("DESC pagination full order = %v, want [P4 P3 P2 P1] (no skip/dup)", ordered)
		}
	})
}

// TestConfGSIScanEskResume: GSI Scan with Limit pagination resumes page 2 via
// LastEvaluatedKey — no skip or duplicate across the page boundary.
func TestConfGSIScanEskResume(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		// Four items with distinct (gsi_pk, gsi_sk) for a deterministic Scan order.
		// Scan order (gsi_pk ASC, gsi_sk ASC): A1,A2,B1,B2.
		for _, it := range []map[string]types.AttributeValue{
			{"pk": sv("A1"), "sk": sv("a1"), "gsi_pk": sv("G1"), "gsi_sk": sv("s1")},
			{"pk": sv("A2"), "sk": sv("a2"), "gsi_pk": sv("G1"), "gsi_sk": sv("s2")},
			{"pk": sv("B1"), "sk": sv("b1"), "gsi_pk": sv("G2"), "gsi_sk": sv("s1")},
			{"pk": sv("B2"), "sk": sv("b2"), "gsi_pk": sv("G2"), "gsi_sk": sv("s2")},
		} {
			putConf(t, c, ctx, "ConfT", it)
		}

		scan := func(start map[string]types.AttributeValue) (*dynamodb.ScanOutput, error) {
			return c.Scan(ctx, &dynamodb.ScanInput{
				TableName:         aws.String("ConfT"),
				IndexName:         aws.String("gsi-all"),
				Limit:             aws.Int32(2),
				ExclusiveStartKey: start,
			})
		}

		// Accumulate pages by resuming via LastEvaluatedKey; assert no skip/dup
		// and that every item is seen exactly once.
		seen := map[string]bool{}
		var seenPks []string
		var start map[string]types.AttributeValue
		pages := 0
		for {
			out, err := scan(start)
			if err != nil {
				t.Fatalf("Scan page %d: %v", pages+1, err)
			}
			pages++
			for _, it := range out.Items {
				if v, ok := it["pk"].(*types.AttributeValueMemberS); ok {
					if seen[v.Value] {
						t.Errorf("duplicate pk %q across pages", v.Value)
					}
					seen[v.Value] = true
					seenPks = append(seenPks, v.Value)
				}
			}
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
		}
		if pages < 2 {
			t.Fatalf("Scan ESK resume produced %d page(s), want >= 2 across a boundary", pages)
		}
		if len(seenPks) != 4 {
			t.Errorf("scan across pages saw %d items, want 4 (no skip): %v", len(seenPks), seenPks)
		}
		for _, w := range []string{"A1", "A2", "B1", "B2"} {
			if !seen[w] {
				t.Errorf("missing pk %q in scan across pages: %v", w, seenPks)
			}
		}
	})
}

// TestConfGSIStaleEsk (G26): after a GSI Query page sets LastEvaluatedKey,
// delete the item that LEK points to, then resume with that stale LEK. The
// resume must not error and must continue (partition restarted from the
// beginning since the pointed-to row is gone).
func TestConfGSIStaleEsk(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfT")
		// ASC order on (G1, gsi_sk): P1,P2,P3,P4. Limit 2 -> page 1 = [P1,P2],
		// LEK points at P2. Delete P2, then resume with the stale LEK.
		for _, it := range []map[string]types.AttributeValue{
			{"pk": sv("P1"), "sk": sv("p1"), "gsi_pk": sv("G1"), "gsi_sk": sv("s1")},
			{"pk": sv("P2"), "sk": sv("p2"), "gsi_pk": sv("G1"), "gsi_sk": sv("s2")},
			{"pk": sv("P3"), "sk": sv("p3"), "gsi_pk": sv("G1"), "gsi_sk": sv("s3")},
			{"pk": sv("P4"), "sk": sv("p4"), "gsi_pk": sv("G1"), "gsi_sk": sv("s4")},
		} {
			putConf(t, c, ctx, "ConfT", it)
		}

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		query := func(start map[string]types.AttributeValue) (*dynamodb.QueryOutput, error) {
			return c.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String("ConfT"),
				IndexName:                 aws.String("gsi-all"),
				KeyConditionExpression:    expr.KeyCondition(),
				ExpressionAttributeNames:  expr.Names(),
				ExpressionAttributeValues: expr.Values(),
				Limit:                     aws.Int32(2),
				ExclusiveStartKey:         start,
			})
		}

		out1, err := query(nil)
		if err != nil {
			t.Fatalf("Query page 1: %v", err)
		}
		if got, want := orderPks(out1.Items), []string{"P1", "P2"}; !equalStrings(got, want) {
			t.Fatalf("page 1 order = %v, want %v", got, want)
		}
		lek := out1.LastEvaluatedKey
		if lek == nil {
			t.Fatalf("page 1 with Limit 2 must set LastEvaluatedKey")
		}

		// Delete the item the LEK points to (P2).
		if _, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String("ConfT"),
			Key:       map[string]types.AttributeValue{"pk": sv("P2"), "sk": sv("p2")},
		}); err != nil {
			t.Fatalf("DeleteItem P2: %v", err)
		}

		// Resume with the now-stale LEK: must NOT error and must continue. The
		// stale ESK restarts the partition, but the resume page is still subject
		// to Limit, so accumulate pages to exhaustion.
		seen := map[string]bool{}
		var seenPks []string
		start := lek
		for {
			out, err := query(start)
			if err != nil {
				t.Fatalf("Query resume with stale LEK errored: %v", err)
			}
			for _, pk := range orderPks(out.Items) {
				seen[pk] = true
				seenPks = append(seenPks, pk)
			}
			if out.LastEvaluatedKey == nil {
				break
			}
			start = out.LastEvaluatedKey
		}
		// Must continue correctly with no error. The exact continuation is target-
		// dependent (the adapter restarts the partition from the beginning,
		// dynamodb-local resumes from after the deleted LEK's sort position), so
		// assert the invariants common to both: the deleted P2 is absent, and the
		// not-yet-returned items after P2 (P3, P4) are present.
		if seen["P2"] {
			t.Errorf("stale-ESK resume unexpectedly returned deleted P2")
		}
		if !seen["P3"] || !seen["P4"] {
			t.Errorf("stale-ESK resume must continue past the deleted key; missing P3/P4 in %v", seenPks)
		}
		for i := 1; i < len(seenPks); i++ {
			for j := 0; j < i; j++ {
				if seenPks[i] == seenPks[j] {
					t.Errorf("stale-ESK resume returned duplicate pk %q: %v", seenPks[i], seenPks)
				}
			}
		}
	})
}

// equalStrings compares two string slices element-wise.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- TTL conformance cases ---

func TestConfUpdateTimeToLive(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "TTLTbl")

		// Enable.
		_, err := c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("expire")},
		})
		if err != nil {
			t.Fatalf("enable: %v", err)
		}
		out, err := c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("TTLTbl")})
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		if out.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusEnabled {
			t.Errorf("status = %v, want ENABLED", out.TimeToLiveDescription.TimeToLiveStatus)
		}
		if aws.ToString(out.TimeToLiveDescription.AttributeName) != "expire" {
			t.Errorf("attr = %q, want expire", aws.ToString(out.TimeToLiveDescription.AttributeName))
		}

		// Disable.
		_, err = c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(false), AttributeName: aws.String("expire")},
		})
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
		out, _ = c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("TTLTbl")})
		if out.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusDisabled {
			t.Errorf("after disable: status = %v, want DISABLED", out.TimeToLiveDescription.TimeToLiveStatus)
		}

		// Re-enable with a different attribute.
		_, err = c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("ttl")},
		})
		if err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		out, _ = c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("TTLTbl")})
		if out.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusEnabled || aws.ToString(out.TimeToLiveDescription.AttributeName) != "ttl" {
			t.Errorf("after re-enable: status=%v attr=%q, want ENABLED/ttl", out.TimeToLiveDescription.TimeToLiveStatus, aws.ToString(out.TimeToLiveDescription.AttributeName))
		}
	})
}

func TestConfUpdateTimeToLiveErrors(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()

		// Nonexistent table -> ResourceNotFoundException (precedence over validation).
		_, err := c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("nope"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("")},
		})
		asResourceNotFound(t, err, "missing table precedence")

		mustCreate(t, c, ctx, "TTLTbl")

		// Empty AttributeName when enabling -> ValidationException.
		_, err = c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("")},
		})
		asValidation(t, err, "empty attr enabling")

		// Empty AttributeName when disabling -> ValidationException (required unconditionally).
		_, err = c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(false), AttributeName: aws.String("")},
		})
		asValidation(t, err, "empty attr disabling")
	})
}

func TestConfDescribeTimeToLive(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "TTLTbl")

		// Never configured -> DISABLED, nil AttributeName.
		out, err := c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("TTLTbl")})
		if err != nil {
			t.Fatalf("describe never-set: %v", err)
		}
		if out.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusDisabled {
			t.Errorf("never-set: status = %v, want DISABLED", out.TimeToLiveDescription.TimeToLiveStatus)
		}
		if out.TimeToLiveDescription.AttributeName != nil {
			t.Errorf("never-set: AttributeName = %v, want nil", out.TimeToLiveDescription.AttributeName)
		}

		// Enable -> ENABLED + attr.
		c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("expire")},
		})
		out, _ = c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("TTLTbl")})
		if out.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusEnabled || aws.ToString(out.TimeToLiveDescription.AttributeName) != "expire" {
			t.Errorf("enabled: status=%v attr=%q, want ENABLED/expire", out.TimeToLiveDescription.TimeToLiveStatus, aws.ToString(out.TimeToLiveDescription.AttributeName))
		}

		// Disable -> DISABLED, nil AttributeName.
		c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(false), AttributeName: aws.String("expire")},
		})
		out, _ = c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("TTLTbl")})
		if out.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusDisabled {
			t.Errorf("after disable: status = %v, want DISABLED", out.TimeToLiveDescription.TimeToLiveStatus)
		}
		if out.TimeToLiveDescription.AttributeName != nil {
			t.Errorf("after disable: AttributeName = %v, want nil", out.TimeToLiveDescription.AttributeName)
		}
	})
}

func TestConfTTLExpiredItemVisible(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "TTLTbl") // pk HASH S, sk RANGE N

		// Enable TTL with attr "expire".
		_, err := c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("TTLTbl"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("expire")},
		})
		if err != nil {
			t.Fatalf("UpdateTimeToLive: %v", err)
		}

		// Put an item whose TTL attr is a past epoch.
		past := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
		putConf(t, c, ctx, "TTLTbl", map[string]types.AttributeValue{
			"pk":     strVal("k1"),
			"sk":     numVal("1"),
			"expire": numVal(past),
		})

		// GetItem returns the (expired) item — no read filtering.
		got, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("TTLTbl"), Key: map[string]types.AttributeValue{"pk": strVal("k1"), "sk": numVal("1")}})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if len(got.Item) == 0 {
			t.Fatal("expired item not visible on GetItem; Faithful read filtering is on")
		}

		// Query includes the expired item.
		q, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("TTLTbl"),
			KeyConditionExpression:    aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strVal("k1")},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(q.Items) != 1 {
			t.Errorf("Query items = %d, want 1 (expired item must be visible)", len(q.Items))
		}

		// Scan includes the expired item.
		sc, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("TTLTbl")})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(sc.Items) != 1 {
			t.Errorf("Scan items = %d, want 1 (expired item must be visible)", len(sc.Items))
		}
	})
}

// =====================================================================
// BatchWriteItem conformance (dual-target)
// =====================================================================

func TestConfBatchWriteMultiTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchA")
		mustCreate(t, c, ctx, "BatchB")

		out, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchA": {batchPut(map[string]types.AttributeValue{"pk": strVal("a1"), "v": strVal("one")})},
			"BatchB": {batchPut(map[string]types.AttributeValue{"pk": strVal("b1"), "v": strVal("two")})},
		}})
		if err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		if len(out.UnprocessedItems) != 0 {
			t.Errorf("UnprocessedItems = %v, want empty", out.UnprocessedItems)
		}
		for table, want := range map[string][2]string{"BatchA": {"a1", "one"}, "BatchB": {"b1", "two"}} {
			got, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: map[string]types.AttributeValue{"pk": strVal(want[0])}})
			if err != nil {
				t.Fatalf("GetItem %s: %v", table, err)
			}
			if v := got.Item["v"].(*types.AttributeValueMemberS).Value; v != want[1] {
				t.Errorf("%s/%s v = %q, want %q", table, want[0], v, want[1])
			}
		}
	})
}

func TestConfBatchWriteCountLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")

		mk := func(n int) []types.WriteRequest {
			reqs := make([]types.WriteRequest, 0, n)
			for i := 0; i < n; i++ {
				reqs = append(reqs, batchPut(map[string]types.AttributeValue{"pk": strVal(fmt.Sprintf("k%02d", i))}))
			}
			return reqs
		}
		if _, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{"BatchT": mk(25)}}); err != nil {
			t.Errorf("25 requests: %v", err)
		}
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{"BatchT": mk(26)}})
		asValidation(t, err, "26 requests")
	})
}

func TestConfBatchWriteDuplicateKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {
				batchPut(map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("one")}),
				batchPut(map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("two")}),
			},
		}})
		asValidation(t, err, "duplicate put keys")
	})
}

func TestConfBatchWritePutDeleteSameKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {
				batchPut(map[string]types.AttributeValue{"pk": strVal("k")}),
				batchDel(map[string]types.AttributeValue{"pk": strVal("k")}),
			},
		}})
		asValidation(t, err, "put+delete same key")
	})
}

func TestConfBatchWriteCrossTableSameKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchA")
		mustCreate(t, c, ctx, "BatchB")
		// Same key value in different tables is NOT a duplicate.
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchA": {batchPut(map[string]types.AttributeValue{"pk": strVal("k")})},
			"BatchB": {batchPut(map[string]types.AttributeValue{"pk": strVal("k")})},
		}})
		if err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		for _, table := range []string{"BatchA", "BatchB"} {
			got, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
			if err != nil || len(got.Item) == 0 {
				t.Errorf("%s/k: item=%v err=%v", table, got.Item, err)
			}
		}
	})
}

func TestConfBatchWriteUnknownTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Good")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"Good": {batchPut(map[string]types.AttributeValue{"pk": strVal("k")})},
			"Nope": {batchPut(map[string]types.AttributeValue{"pk": strVal("k")})},
		}})
		asResourceNotFound(t, err, "unknown table in batch")
		// No partial processing: the valid table's item was NOT written.
		got, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("Good"), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if len(got.Item) != 0 {
			t.Errorf("valid table written despite rejected batch: %v", got.Item)
		}
	})
}

func TestConfBatchWriteBadKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {
				batchPut(map[string]types.AttributeValue{"pk": strVal("good")}),
				batchPut(map[string]types.AttributeValue{"v": strVal("no partition key")}),
			},
		}})
		asValidation(t, err, "item missing partition key")
		got, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchT"), Key: map[string]types.AttributeValue{"pk": strVal("good")}})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if len(got.Item) != 0 {
			t.Errorf("valid request written despite rejected batch: %v", got.Item)
		}
	})
}

func TestConfBatchWriteEmptyRequestItems(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{}})
		asValidation(t, err, "empty RequestItems")
	})
}

func TestConfBatchWriteMixed(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchA")
		mustCreate(t, c, ctx, "BatchB")
		putConf(t, c, ctx, "BatchA", map[string]types.AttributeValue{"pk": strVal("del")})

		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchA": {
				batchPut(map[string]types.AttributeValue{"pk": strVal("put1")}),
				batchDel(map[string]types.AttributeValue{"pk": strVal("del")}),
			},
			"BatchB": {batchPut(map[string]types.AttributeValue{"pk": strVal("keep")})},
		}})
		if err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		got, _ := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchA"), Key: map[string]types.AttributeValue{"pk": strVal("put1")}})
		if len(got.Item) == 0 {
			t.Error("BatchA/put1 missing")
		}
		got, _ = c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchA"), Key: map[string]types.AttributeValue{"pk": strVal("del")}})
		if len(got.Item) != 0 {
			t.Errorf("BatchA/del should be deleted: %v", got.Item)
		}
		got, _ = c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchB"), Key: map[string]types.AttributeValue{"pk": strVal("keep")}})
		if len(got.Item) == 0 {
			t.Error("BatchB/keep missing")
		}
	})
}

func TestConfBatchWriteDelete(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		for _, k := range []string{"k1", "k2", "k3"} {
			putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal(k)})
		}
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {
				batchDel(map[string]types.AttributeValue{"pk": strVal("k1")}),
				batchDel(map[string]types.AttributeValue{"pk": strVal("k2")}),
			},
		}})
		if err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		for _, k := range []string{"k1", "k2"} {
			got, _ := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchT"), Key: map[string]types.AttributeValue{"pk": strVal(k)}})
			if len(got.Item) != 0 {
				t.Errorf("%s should be deleted: %v", k, got.Item)
			}
		}
		got, _ := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchT"), Key: map[string]types.AttributeValue{"pk": strVal("k3")}})
		if len(got.Item) == 0 {
			t.Error("k3 should remain")
		}
	})
}

func TestConfBatchWriteOverwrite(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("first")})
		if _, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {batchPut(map[string]types.AttributeValue{"pk": strVal("k"), "v": strVal("second")})},
		}}); err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		got, _ := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchT"), Key: map[string]types.AttributeValue{"pk": strVal("k")}})
		if v := got.Item["v"].(*types.AttributeValueMemberS).Value; v != "second" {
			t.Errorf("v = %q, want second", v)
		}
	})
}

func TestConfBatchWriteGsiPut(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "BatchG")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchG": {
				batchPut(map[string]types.AttributeValue{"pk": strVal("p1"), "sk": strVal("s1"), "gsi_pk": strVal("G1"), "gsi_sk": strVal("a")}),
				batchPut(map[string]types.AttributeValue{"pk": strVal("p2"), "sk": strVal("s2"), "gsi_pk": strVal("G1"), "gsi_sk": strVal("b")}),
				batchPut(map[string]types.AttributeValue{"pk": strVal("p3"), "sk": strVal("s3")}), // sparse: not indexed
			},
		}})
		if err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		q, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("BatchG"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    aws.String("gsi_pk = :g"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":g": strVal("G1")},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if q.Count != 2 {
			t.Errorf("GSI count = %d, want 2 (sparse item not indexed)", q.Count)
		}
	})
}

func TestConfBatchWriteGsiDelete(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "BatchG")
		if _, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchG": {
				batchPut(map[string]types.AttributeValue{"pk": strVal("p1"), "sk": strVal("s1"), "gsi_pk": strVal("G1"), "gsi_sk": strVal("a")}),
				batchPut(map[string]types.AttributeValue{"pk": strVal("p2"), "sk": strVal("s2"), "gsi_pk": strVal("G1"), "gsi_sk": strVal("b")}),
			},
		}}); err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		if _, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchG": {batchDel(map[string]types.AttributeValue{"pk": strVal("p1"), "sk": strVal("s1")})},
		}}); err != nil {
			t.Fatalf("BatchWriteItem delete: %v", err)
		}
		q, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("BatchG"),
			IndexName:                 aws.String("gsi-all"),
			KeyConditionExpression:    aws.String("gsi_pk = :g"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":g": strVal("G1")},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if q.Count != 1 {
			t.Errorf("GSI count after delete = %d, want 1", q.Count)
		}
	})
}

func TestConfBatchWriteGsiOverwrite(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "BatchG")
		putConf(t, c, ctx, "BatchG", map[string]types.AttributeValue{"pk": strVal("p1"), "sk": strVal("s1"), "gsi_pk": strVal("G1"), "gsi_sk": strVal("a")})

		// Batch overwrite with CHANGED GSI key attrs: old index row must go.
		if _, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchG": {batchPut(map[string]types.AttributeValue{"pk": strVal("p1"), "sk": strVal("s1"), "gsi_pk": strVal("G2"), "gsi_sk": strVal("b")})},
		}}); err != nil {
			t.Fatalf("BatchWriteItem: %v", err)
		}
		count := func(val string) int32 {
			t.Helper()
			q, err := c.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String("BatchG"),
				IndexName:                 aws.String("gsi-all"),
				KeyConditionExpression:    aws.String("gsi_pk = :g"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":g": strVal(val)},
			})
			if err != nil {
				t.Fatalf("Query %s: %v", val, err)
			}
			return q.Count
		}
		if n := count("G1"); n != 0 {
			t.Errorf("old GSI row: count = %d, want 0 (no phantom hits)", n)
		}
		if n := count("G2"); n != 1 {
			t.Errorf("new GSI row: count = %d, want 1", n)
		}
	})
}

func TestConfBatchWriteItemTooLarge(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {
				batchPut(map[string]types.AttributeValue{"pk": strVal("good")}),
				batchPut(map[string]types.AttributeValue{"pk": strVal("big"), "data": strVal(strings.Repeat("x", 400*1024+1))}),
			},
		}})
		asValidation(t, err, "oversized item")
		// All-or-nothing: the valid put was not written either.
		got, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("BatchT"), Key: map[string]types.AttributeValue{"pk": strVal("good")}})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if len(got.Item) != 0 {
			t.Errorf("valid request written despite rejected batch: %v", got.Item)
		}
	})
}

// All three return ValidationException against dynamodb-local 3.3.1.
func TestConfBatchWriteNeitherPutNorDelete(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {types.WriteRequest{}},
		}})
		asValidation(t, err, "WriteRequest with neither Put nor Delete")
	})
}

func TestConfBatchWriteBothPutAndDelete(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"BatchT": {{
				PutRequest:    &types.PutRequest{Item: map[string]types.AttributeValue{"pk": strVal("k")}},
				DeleteRequest: &types.DeleteRequest{Key: map[string]types.AttributeValue{"pk": strVal("k")}},
			}},
		}})
		asValidation(t, err, "WriteRequest with both Put and Delete")
	})
}

func TestConfBatchWriteEmptyTableRequests(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{"BatchT": {}}})
		asValidation(t, err, "table with empty request list")
	})
}

func TestConfBatchWriteEmptyTableName(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
			"": {batchPut(map[string]types.AttributeValue{"pk": strVal("k")})},
		}})
		asValidation(t, err, "empty table name in batch write")
	})
}

// =====================================================================
// BatchGetItem conformance (dual-target)
// =====================================================================

func TestConfBatchGetMultiTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchA")
		mustCreate(t, c, ctx, "BatchB")
		putConf(t, c, ctx, "BatchA", map[string]types.AttributeValue{"pk": strVal("a1"), "v": strVal("one"), "n": numVal("12.5")})
		putConf(t, c, ctx, "BatchB", map[string]types.AttributeValue{"pk": strVal("b1"), "v": strVal("two")})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchA": {Keys: []map[string]types.AttributeValue{{"pk": strVal("a1")}}},
			"BatchB": {Keys: []map[string]types.AttributeValue{{"pk": strVal("b1")}}},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		if len(out.UnprocessedKeys) != 0 {
			t.Errorf("UnprocessedKeys = %v, want empty", out.UnprocessedKeys)
		}
		if len(out.Responses["BatchA"]) != 1 || len(out.Responses["BatchB"]) != 1 {
			t.Fatalf("Responses sizes: A=%d B=%d, want 1/1", len(out.Responses["BatchA"]), len(out.Responses["BatchB"]))
		}
		if v := out.Responses["BatchA"][0]["n"].(*types.AttributeValueMemberN).Value; v != "12.5" {
			t.Errorf("A item n = %q, want 12.5", v)
		}
		if v := out.Responses["BatchB"][0]["v"].(*types.AttributeValueMemberS).Value; v != "two" {
			t.Errorf("B item v = %q, want two", v)
		}
	})
}

func TestConfBatchGetCountLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")

		mk := func(n int) []map[string]types.AttributeValue {
			keys := make([]map[string]types.AttributeValue, 0, n)
			for i := 0; i < n; i++ {
				keys = append(keys, map[string]types.AttributeValue{"pk": strVal(fmt.Sprintf("k%03d", i))})
			}
			return keys
		}
		if _, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{"BatchT": {Keys: mk(100)}}}); err != nil {
			t.Errorf("100 keys: %v", err)
		}
		_, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{"BatchT": {Keys: mk(101)}}})
		asValidation(t, err, "101 keys")
	})
}

func TestConfBatchGetNonexistentKey(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal("real")})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {Keys: []map[string]types.AttributeValue{{"pk": strVal("real")}, {"pk": strVal("ghost")}}},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		if len(out.Responses["BatchT"]) != 1 {
			t.Fatalf("len(Responses[T]) = %d, want 1 (ghost omitted)", len(out.Responses["BatchT"]))
		}
		if pk := out.Responses["BatchT"][0]["pk"].(*types.AttributeValueMemberS).Value; pk != "real" {
			t.Errorf("pk = %q, want real", pk)
		}
		if len(out.UnprocessedKeys) != 0 {
			t.Errorf("UnprocessedKeys = %v, want empty (missing keys are NOT unprocessed)", out.UnprocessedKeys)
		}
	})
}

func TestConfBatchGetDuplicateKeys(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {Keys: []map[string]types.AttributeValue{{"pk": strVal("k")}, {"pk": strVal("k")}}},
		}})
		asValidation(t, err, "duplicate keys")
	})
}

func TestConfBatchGetOrdering(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		for _, k := range []string{"k1", "k2", "k3", "k5"} {
			putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal(k)})
		}
		// Shuffled request order with an interleaved miss.
		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {Keys: []map[string]types.AttributeValue{
				{"pk": strVal("k3")}, {"pk": strVal("k1")}, {"pk": strVal("k5")}, {"pk": strVal("ghost")}, {"pk": strVal("k2")},
			}},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		// dynamodb-local returns the four found keys in an arbitrary internal
		// (non-sorted, non-request) order; the mock deterministically sorts.
		// Both agree on the SET of returned keys with the ghost omitted, so
		// assert the set, not the order (order is not a dynamodb-local contract).
		want := map[string]bool{"k1": true, "k2": true, "k3": true, "k5": true}
		got := out.Responses["BatchT"]
		if len(got) != len(want) {
			t.Fatalf("len(Responses[T]) = %d, want %d", len(got), len(want))
		}
		gotSet := map[string]bool{}
		for _, it := range got {
			gotSet[it["pk"].(*types.AttributeValueMemberS).Value] = true
		}
		for w := range want {
			if !gotSet[w] {
				t.Errorf("Responses[T] missing requested key %q (ghost must be omitted); got %v", w, gotSet)
			}
		}
	})
}

func TestConfBatchGetUnknownTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Good")
		_, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"Good": {Keys: []map[string]types.AttributeValue{{"pk": strVal("k")}}},
			"Nope": {Keys: []map[string]types.AttributeValue{{"pk": strVal("k")}}},
		}})
		asResourceNotFound(t, err, "unknown table in batch")
	})
}

func TestConfBatchGetConsistentReadPerTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchA")
		mustCreate(t, c, ctx, "BatchB")
		putConf(t, c, ctx, "BatchA", map[string]types.AttributeValue{"pk": strVal("a1")})
		putConf(t, c, ctx, "BatchB", map[string]types.AttributeValue{"pk": strVal("b1")})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchA": {Keys: []map[string]types.AttributeValue{{"pk": strVal("a1")}}, ConsistentRead: aws.Bool(true)},
			"BatchB": {Keys: []map[string]types.AttributeValue{{"pk": strVal("b1")}}},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		if len(out.Responses["BatchA"]) != 1 || len(out.Responses["BatchB"]) != 1 {
			t.Errorf("Responses sizes: A=%d B=%d, want 1/1", len(out.Responses["BatchA"]), len(out.Responses["BatchB"]))
		}
	})
}

func TestConfBatchGetCompositeKeys(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "BatchT") // pk HASH S, sk RANGE N
		key := func(sk string) map[string]types.AttributeValue {
			return map[string]types.AttributeValue{"pk": strVal("p1"), "sk": numVal(sk)}
		}
		putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal("p1"), "sk": numVal("1")})
		putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal("p1"), "sk": numVal("2")})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {Keys: []map[string]types.AttributeValue{key("1"), key("99"), key("2")}},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		if len(out.Responses["BatchT"]) != 2 {
			t.Errorf("len(Responses[T]) = %d, want 2 (missing (p1,99) omitted)", len(out.Responses["BatchT"]))
		}
	})
}

func TestConfBatchGetEmptyRequestItems(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{}})
		asValidation(t, err, "empty RequestItems")
	})
}

func TestConfBatchGetAllMissTableOmitted(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchA")
		mustCreate(t, c, ctx, "BatchB")
		putConf(t, c, ctx, "BatchA", map[string]types.AttributeValue{"pk": strVal("a1")})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchA": {Keys: []map[string]types.AttributeValue{{"pk": strVal("a1")}}},
			"BatchB": {Keys: []map[string]types.AttributeValue{{"pk": strVal("ghost")}}},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		if len(out.Responses["BatchA"]) != 1 {
			t.Errorf("len(Responses[A]) = %d, want 1", len(out.Responses["BatchA"]))
		}
		if len(out.Responses["BatchB"]) != 0 {
			t.Errorf("len(Responses[B]) = %d, want 0 (empty entry, matching dynamodb-local)", len(out.Responses["BatchB"]))
		}
	})
}

// Empty Keys returns ValidationException against dynamodb-local 3.3.1.
func TestConfBatchGetEmptyTableKeys(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {Keys: []map[string]types.AttributeValue{}},
		}})
		asValidation(t, err, "table with empty Keys list")
	})
}

func TestConfBatchGetEmptyTableName(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"": {Keys: []map[string]types.AttributeValue{{"pk": strVal("k")}}},
		}})
		asValidation(t, err, "empty table name in batch get")
	})
}

// TTL: expired items are visible to BatchGetItem.
func TestConfBatchGetExpiredItemVisible(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")

		if _, err := c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName:               aws.String("BatchT"),
			TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("expire")},
		}); err != nil {
			t.Fatalf("UpdateTimeToLive: %v", err)
		}
		past := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
		putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal("k"), "expire": numVal(past)})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {Keys: []map[string]types.AttributeValue{{"pk": strVal("k")}}},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		if len(out.Responses["BatchT"]) != 1 {
			t.Errorf("len(Responses[T]) = %d, want 1 (expired item visible)", len(out.Responses["BatchT"]))
		}
	})
}

// =====================================================================
// BatchGetItem projection conformance (dual-target)
// =====================================================================

func TestConfBatchGetProjection(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal("k1"), "v": strVal("one"), "n": numVal("12.5")})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {
				Keys:                 []map[string]types.AttributeValue{{"pk": strVal("k1")}},
				ProjectionExpression: aws.String("v"),
			},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		got := out.Responses["BatchT"]
		if len(got) != 1 {
			t.Fatalf("len(Responses[BatchT]) = %d, want 1", len(got))
		}
		if len(got[0]) != 1 || got[0]["v"].(*types.AttributeValueMemberS).Value != "one" {
			t.Errorf("item = %v, want only {v:one} (keys not auto-returned)", got[0])
		}
	})
}

func TestConfBatchGetProjectionNameSubstitution(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		putConf(t, c, ctx, "BatchT", map[string]types.AttributeValue{"pk": strVal("k1"), "v": strVal("one"), "n": numVal("12.5")})

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {
				Keys:                     []map[string]types.AttributeValue{{"pk": strVal("k1")}},
				ProjectionExpression:     aws.String("#x, n"),
				ExpressionAttributeNames: map[string]string{"#x": "v"},
			},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		got := out.Responses["BatchT"]
		if len(got) != 1 || len(got[0]) != 2 {
			t.Fatalf("item = %v, want one {v, n} item", got)
		}
		if got[0]["v"].(*types.AttributeValueMemberS).Value != "one" || got[0]["n"].(*types.AttributeValueMemberN).Value != "12.5" {
			t.Errorf("item = %v, want {v:one, n:12.5}", got[0])
		}
	})
}

func TestConfBatchGetExpressionNamesWithoutProjection(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "BatchT")
		_, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"BatchT": {
				Keys:                     []map[string]types.AttributeValue{{"pk": strVal("k1")}},
				ExpressionAttributeNames: map[string]string{"#x": "v"},
			},
		}})
		asValidation(t, err, "ExpressionAttributeNames without projection (unused)")
	})
}

// --- BatchGetItem 16MiB response cap ---

// seedCapItems writes n {"pk","big"} items (k00..k{n-1}) to table in
// BatchWriteItem chunks of 25. Each item's accounting size is exactly
// 8+payloadLen bytes (len("pk")+3 for the key, len("big")+payloadLen).
func seedCapItems(t *testing.T, c api, ctx context.Context, table string, n, payloadLen int) {
	t.Helper()
	payload := strings.Repeat("x", payloadLen)
	for start := 0; start < n; start += 25 {
		end := start + 25
		if end > n {
			end = n
		}
		reqs := make([]types.WriteRequest, 0, end-start)
		for i := start; i < end; i++ {
			reqs = append(reqs, batchPut(map[string]types.AttributeValue{
				"pk":  strVal(fmt.Sprintf("k%02d", i)),
				"big": strVal(payload),
			}))
		}
		if _, err := c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{table: reqs}}); err != nil {
			t.Fatalf("BatchWriteItem seed: %v", err)
		}
	}
}

// capKeys builds n SDK request keys k00..k{n-1}.
func capKeys(n int) []map[string]types.AttributeValue {
	keys := make([]map[string]types.AttributeValue, 0, n)
	for i := 0; i < n; i++ {
		keys = append(keys, map[string]types.AttributeValue{"pk": strVal(fmt.Sprintf("k%02d", i))})
	}
	return keys
}

// 100 items over the 16MiB cap: floor(16MiB / per-item) returned, the rest
// spilled. Counts only — the spilled key set is not a stable contract.
func TestConfBatchGetResponseCap(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "CapT")
		const payloadLen = 170000 // per-item W1 size: 8 + 170000 = 170,008
		seedCapItems(t, c, ctx, "CapT", 100, payloadLen)

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"CapT": {Keys: capKeys(100)},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		wantReturned := 16 * 1024 * 1024 / (8 + payloadLen) // 98
		if got := len(out.Responses["CapT"]); got != wantReturned {
			t.Errorf("len(Responses[CapT]) = %d, want %d", got, wantReturned)
		}
		if got := len(out.UnprocessedKeys["CapT"].Keys); got != 100-wantReturned {
			t.Errorf("len(UnprocessedKeys[CapT].Keys) = %d, want %d", got, 100-wantReturned)
		}
		if got := len(out.Responses["CapT"]) + len(out.UnprocessedKeys["CapT"].Keys); got != 100 {
			t.Errorf("returned + unprocessed = %d, want 100", got)
		}
	})
}

// Measurement is pre-projection: projecting to the tiny key
// attribute changes the response bodies but not which keys spill.
func TestConfBatchGetResponseCapPreProjection(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "CapP")
		const payloadLen = 170000
		seedCapItems(t, c, ctx, "CapP", 100, payloadLen)

		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"CapP": {Keys: capKeys(100), ProjectionExpression: aws.String("pk")},
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		wantReturned := 16 * 1024 * 1024 / (8 + payloadLen)
		if got := len(out.Responses["CapP"]); got != wantReturned {
			t.Errorf("len(Responses[CapP]) = %d, want %d (pre-projection measurement)", got, wantReturned)
		}
		if got := len(out.UnprocessedKeys["CapP"].Keys); got != 100-wantReturned {
			t.Errorf("len(UnprocessedKeys[CapP].Keys) = %d, want %d", got, 100-wantReturned)
		}
		for i, item := range out.Responses["CapP"] {
			if len(item) != 1 {
				t.Errorf("returned item[%d] has %d attrs, want 1 (pk only)", i, len(item))
			}
		}
	})
}

// The budget is whole-response: one accumulator across tables. The per-table
// split is arbitrary on the reference — assert only the totals, the per-table
// returned+unprocessed invariant, and the spill echo shape.
func TestConfBatchGetResponseCapCrossTableEcho(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "CapA")
		mustCreate(t, c, ctx, "CapB")
		const payloadLen = 300000 // per-item W1 size: 8 + 300000 = 300,008
		seedCapItems(t, c, ctx, "CapA", 50, payloadLen)
		seedCapItems(t, c, ctx, "CapB", 50, payloadLen)

		kaA := types.KeysAndAttributes{
			Keys:                     capKeys(50),
			ConsistentRead:           aws.Bool(true),
			ProjectionExpression:     aws.String("#b"),
			ExpressionAttributeNames: map[string]string{"#b": "big"},
		}
		kaB := types.KeysAndAttributes{
			Keys:                     capKeys(50),
			ConsistentRead:           aws.Bool(true),
			ProjectionExpression:     aws.String("#b"),
			ExpressionAttributeNames: map[string]string{"#b": "big"},
		}
		out, err := c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
			"CapA": kaA,
			"CapB": kaB,
		}})
		if err != nil {
			t.Fatalf("BatchGetItem: %v", err)
		}
		wantTotal := 16 * 1024 * 1024 / (8 + payloadLen) // 55
		gotReturned := len(out.Responses["CapA"]) + len(out.Responses["CapB"])
		gotSpilled := len(out.UnprocessedKeys["CapA"].Keys) + len(out.UnprocessedKeys["CapB"].Keys)
		if gotReturned != wantTotal {
			t.Errorf("total returned = %d, want %d", gotReturned, wantTotal)
		}
		if gotSpilled != 100-wantTotal {
			t.Errorf("total spilled = %d, want %d", gotSpilled, 100-wantTotal)
		}
		for _, table := range []string{"CapA", "CapB"} {
			if got := len(out.Responses[table]) + len(out.UnprocessedKeys[table].Keys); got != 50 {
				t.Errorf("%s: returned + unprocessed = %d, want 50", table, got)
			}
		}
		// Spilled entries echo the request's ConsistentRead, projection, and
		// ExpressionAttributeNames.
		for table, sp := range out.UnprocessedKeys {
			if !aws.ToBool(sp.ConsistentRead) {
				t.Errorf("%s: spilled ConsistentRead = %v, want true", table, sp.ConsistentRead)
			}
			if got := aws.ToString(sp.ProjectionExpression); got != "#b" {
				t.Errorf("%s: spilled ProjectionExpression = %q, want #b", table, got)
			}
			if sp.ExpressionAttributeNames["#b"] != "big" {
				t.Errorf("%s: spilled ExpressionAttributeNames = %v, want {#b:big}", table, sp.ExpressionAttributeNames)
			}
		}
	})
}

// =====================================================================
// UpdateTable conformance (dual-target)
// =====================================================================

// waitForGsiActive polls DescribeTable until the named GSI's IndexStatus is
// ACTIVE, failing the test on a 10s timeout. The adapter is ACTIVE immediately;
// dynamodb-local activates a small-table GSI in seconds. A timeout is a real
// failure, never a skip.
func waitForGsiActive(t *testing.T, c api, ctx context.Context, table, gsi string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
		if err != nil {
			t.Fatalf("waitForGsiActive DescribeTable %q: %v", table, err)
		}
		for _, g := range out.Table.GlobalSecondaryIndexes {
			if aws.ToString(g.IndexName) == gsi && g.IndexStatus == types.IndexStatusActive {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForGsiActive: %q on %q never reached ACTIVE within 10s", gsi, table)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestConfUpdateTableGsiAddBackfill: seed items, UpdateTable create GSI, poll
// until ACTIVE, then Query and assert the backfilled items match (sparse absent).
func TestConfUpdateTableGsiAddBackfill(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "UpdT") // pk HASH S
		// Seed: two indexable, one sparse.
		putConf(t, c, ctx, "UpdT", map[string]types.AttributeValue{
			"pk": sv("A"), "gp": sv("G1"), "gr": sv("s1"),
		})
		putConf(t, c, ctx, "UpdT", map[string]types.AttributeValue{
			"pk": sv("B"), "gp": sv("G1"), "gr": sv("s2"),
		})
		putConf(t, c, ctx, "UpdT", map[string]types.AttributeValue{
			"pk": sv("D"), // sparse: no gp
		})

		_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
			TableName: aws.String("UpdT"),
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gr"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
				Create: &types.CreateGlobalSecondaryIndexAction{
					IndexName: aws.String("g1a"),
					KeySchema: []types.KeySchemaElement{
						{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash},
						{AttributeName: aws.String("gr"), KeyType: types.KeyTypeRange},
					},
					Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
				},
			}},
		})
		if err != nil {
			t.Fatalf("UpdateTable create: %v", err)
		}
		waitForGsiActive(t, c, ctx, "UpdT", "g1a")

		q, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("UpdT"),
			IndexName:                 aws.String("g1a"),
			KeyConditionExpression:    aws.String("gp = :v"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": &types.AttributeValueMemberS{Value: "G1"}},
			ScanIndexForward:          aws.Bool(true),
		})
		if err != nil {
			t.Fatalf("Query new GSI: %v", err)
		}
		if len(q.Items) != 2 {
			t.Fatalf("backfill query returned %d items, want 2", len(q.Items))
		}
		got := []string{
			q.Items[0]["pk"].(*types.AttributeValueMemberS).Value,
			q.Items[1]["pk"].(*types.AttributeValueMemberS).Value,
		}
		want := []string{"A", "B"}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("item[%d].pk = %q, want %q", i, got[i], w)
			}
		}
	})
}

// TestConfUpdateTableGsiDelete: create a GSI, delete it, assert DescribeTable
// omits it and Query with that IndexName fails.
func TestConfUpdateTableGsiDelete(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "UpdT")
		// Create then immediately delete the GSI.
		if _, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
			TableName: aws.String("UpdT"),
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
				Create: &types.CreateGlobalSecondaryIndexAction{
					IndexName:  aws.String("g1a"),
					KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
					Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
				},
			}},
		}); err != nil {
			t.Fatalf("UpdateTable create: %v", err)
		}
		// dynamodb-local creates the GSI asynchronously; wait until ACTIVE
		// before deleting, or the delete races the CREATING window.
		waitForGsiActive(t, c, ctx, "UpdT", "g1a")
		if _, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
			TableName: aws.String("UpdT"),
			GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
				Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g1a")},
			}},
		}); err != nil {
			t.Fatalf("UpdateTable delete: %v", err)
		}
		// dynamodb-local may have a brief DELETING window; poll until gone.
		deadline := time.Now().Add(10 * time.Second)
		for {
			out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("UpdT")})
			if err != nil {
				t.Fatalf("delete-poll DescribeTable: %v", err)
			}
			gone := true
			for _, g := range out.Table.GlobalSecondaryIndexes {
				if aws.ToString(g.IndexName) == "g1a" {
					gone = false
				}
			}
			if gone || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		desc, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("UpdT")})
		if err != nil {
			t.Fatalf("final DescribeTable: %v", err)
		}
		for _, g := range desc.Table.GlobalSecondaryIndexes {
			if aws.ToString(g.IndexName) == "g1a" {
				t.Errorf("GSI g1a still present after delete")
			}
		}
		_, qerr := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("UpdT"),
			IndexName:                 aws.String("g1a"),
			KeyConditionExpression:    aws.String("gp = :v"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": &types.AttributeValueMemberS{Value: "x"}},
		})
		if qerr == nil {
			t.Error("Query on deleted GSI should fail")
		}
	})
}

// TestConfUpdateTableIgnoredNoOp: BillingMode + ProvisionedThroughput with no
// GSI updates is accepted-and-ignored; DescribeTable reflects no change.
func TestConfUpdateTableIgnoredNoOp(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "UpdT")
		if _, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
			TableName:             aws.String("UpdT"),
			BillingMode:           types.BillingModePayPerRequest,
			ProvisionedThroughput: &types.ProvisionedThroughput{ReadCapacityUnits: aws.Int64(5), WriteCapacityUnits: aws.Int64(5)},
		}); err != nil {
			t.Fatalf("throughput-only UpdateTable: %v", err)
		}
		desc, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("UpdT")})
		if err != nil {
			t.Fatalf("DescribeTable: %v", err)
		}
		if len(desc.Table.GlobalSecondaryIndexes) != 0 {
			t.Errorf("no-op added GSIs: %v", desc.Table.GlobalSecondaryIndexes)
		}
	})
}

func TestConfUpdateTableEmptyUpdates(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "UpdT")
		_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{TableName: aws.String("UpdT")})
		asValidation(t, err, "empty UpdateTable")
	})
}

func TestConfUpdateTableUnknownTable(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
			TableName: aws.String("missing"),
			GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
				Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g1")},
			}},
		})
		asResourceNotFound(t, err, "UpdateTable unknown table")
	})
}

// UpdateTable Create naming an existing index AND carrying an invalid key
// schema surfaces the existing-index error, not the schema error. Both
// targets return an existing-index error; asserted by message since
// dynamodb-local returns ValidationException where the adapter returns
// ResourceInUseException.
func TestConfUpdateTableCreateExistingPrecedence(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("UtPrec"),
			KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gk"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("gsi1"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			}},
			BillingMode: types.BillingModePayPerRequest,
		})
		if err != nil {
			t.Fatalf("CreateTable: %v", err)
		}

		// Same index name + invalid key schema (same attribute as HASH and RANGE).
		_, err = c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
			TableName: aws.String("UtPrec"),
			GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
				Create: &types.CreateGlobalSecondaryIndexAction{
					IndexName: aws.String("gsi1"),
					KeySchema: []types.KeySchemaElement{
						{AttributeName: aws.String("gk"), KeyType: types.KeyTypeHash},
						{AttributeName: aws.String("gk"), KeyType: types.KeyTypeRange},
					},
					Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
				},
			}},
		})
		if err == nil {
			t.Fatal("UpdateTable: expected existing-index error, got nil")
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Errorf("UpdateTable err = %v, want existing-index error (message contains \"already exists\")", err)
		}
	})
}

// The following cases assert restrictive behaviors: the engine follows
// documented AWS where dynamodb-local may be more permissive, so they
// run against the adapter only.
func TestAdapterUpdateTableAddExisting(t *testing.T) {
	ctx := context.Background()
	c, cleanup := newAdapterTarget(t)
	defer cleanup()
	mustCreate(t, c, ctx, "UpdT")
	// Create g1x once.
	c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{
				IndexName:  aws.String("g1x"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		}},
	})
	// Create g1x again -> ResourceInUseException.
	_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{
				IndexName:  aws.String("g1x"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		}},
	})
	asResourceInUse(t, err, "add existing GSI")
}

func TestAdapterUpdateTableDeleteUnknown(t *testing.T) {
	ctx := context.Background()
	c, cleanup := newAdapterTarget(t)
	defer cleanup()
	mustCreate(t, c, ctx, "UpdT")
	_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("nope")},
		}},
	})
	asResourceNotFound(t, err, "delete unknown GSI")
}

func TestAdapterUpdateTable21stGsi(t *testing.T) {
	ctx := context.Background()
	c, cleanup := newAdapterTarget(t)
	defer cleanup()
	mustCreate(t, c, ctx, "UpdT")
	// Create 20 GSIs (cap enforced; adapter-only).
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("g%02d", i)
		if _, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
			TableName: aws.String("UpdT"),
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String(name), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
				Create: &types.CreateGlobalSecondaryIndexAction{
					IndexName:  aws.String(name),
					KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String(name), KeyType: types.KeyTypeHash}},
					Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
				},
			}},
		}); err != nil {
			t.Fatalf("create GSI %d: %v", i, err)
		}
	}
	_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp21"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{
				IndexName:  aws.String("g21"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp21"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
			},
		}},
	})
	asLimitExceeded(t, err, "21st GSI")
}

func TestAdapterUpdateTableTwoActions(t *testing.T) {
	ctx := context.Background()
	c, cleanup := newAdapterTarget(t)
	defer cleanup()
	mustCreate(t, c, ctx, "UpdT")
	_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("a")}},
			{Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("b")}},
		},
	})
	asValidation(t, err, "two GSI actions")
}

func TestAdapterUpdateTableGsiPlusBillingMode(t *testing.T) {
	ctx := context.Background()
	c, cleanup := newAdapterTarget(t)
	defer cleanup()
	mustCreate(t, c, ctx, "UpdT")
	_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:   aws.String("UpdT"),
		BillingMode: types.BillingModePayPerRequest,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{
				IndexName:  aws.String("g1x"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		}},
	})
	asValidation(t, err, "GSI action + BillingMode")
}

func TestAdapterUpdateTableThroughputUpdateCombos(t *testing.T) {
	ctx := context.Background()
	c, cleanup := newAdapterTarget(t)
	defer cleanup()
	mustCreate(t, c, ctx, "UpdT")
	// Two Update actions.
	_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{Update: &types.UpdateGlobalSecondaryIndexAction{IndexName: aws.String("g1")}},
			{Update: &types.UpdateGlobalSecondaryIndexAction{IndexName: aws.String("g2")}},
		},
	})
	asValidation(t, err, "two Update actions")
	// Update + Create.
	_, err = c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{Update: &types.UpdateGlobalSecondaryIndexAction{IndexName: aws.String("g1")}},
			{Create: &types.CreateGlobalSecondaryIndexAction{IndexName: aws.String("g2")}},
		},
	})
	asValidation(t, err, "Update + Create")
	// Update + BillingMode.
	_, err = c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:   aws.String("UpdT"),
		BillingMode: types.BillingModePayPerRequest,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Update: &types.UpdateGlobalSecondaryIndexAction{IndexName: aws.String("g1")},
		}},
	})
	asValidation(t, err, "Update + BillingMode")
}

func TestAdapterUpdateTableStrayAttributeDefinitions(t *testing.T) {
	ctx := context.Background()
	c, cleanup := newAdapterTarget(t)
	defer cleanup()
	mustCreate(t, c, ctx, "UpdT")
	// With a Delete action.
	_, err := c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("UpdT"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g1")},
		}},
	})
	asValidation(t, err, "stray AttributeDefinitions with Delete")
	// With no GSI action (ignored-fields only).
	_, err = c.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:   aws.String("UpdT"),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
	})
	asValidation(t, err, "stray AttributeDefinitions with no GSI action")
}

// --- projection helpers ---

// projConfSeed puts one rich item (pk P1, scalars + nested map + list) for
// projection conformance cases.
func projConfSeed(t *testing.T, c api, ctx context.Context, table string) {
	t.Helper()
	putConf(t, c, ctx, table, map[string]types.AttributeValue{
		"pk":  strVal("P1"),
		"top": strVal("topval"),
		"num": numVal("42"),
		"obj": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"a": strVal("aval"),
			"b": strVal("bval"),
			"nested": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
				"x": strVal("xval"),
				"y": strVal("yval"),
			}},
		}},
		"arr": &types.AttributeValueMemberL{Value: []types.AttributeValue{
			strVal("e0"), strVal("e1"), strVal("e2"),
		}},
	})
}

// wantAttrs asserts the item has exactly the given attribute names (sorted).
func wantAttrs(t *testing.T, item map[string]types.AttributeValue, want ...string) {
	t.Helper()
	got := itemAttrNamesConf(item)
	if len(got) != len(want) {
		t.Fatalf("attrs = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("attrs = %v, want %v", got, want)
		}
	}
}

// projGetConf is GetItem with a projection, failing on error.
func projGetConf(t *testing.T, c api, ctx context.Context, table, projExpr string, names map[string]string) map[string]types.AttributeValue {
	t.Helper()
	in := &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key:       map[string]types.AttributeValue{"pk": strVal("P1")},
	}
	if projExpr != "" {
		in.ProjectionExpression = aws.String(projExpr)
	}
	if names != nil {
		in.ExpressionAttributeNames = names
	}
	out, err := c.GetItem(ctx, in)
	if err != nil {
		t.Fatalf("GetItem proj=%q: %v", projExpr, err)
	}
	return out.Item
}

// --- GetItem projection semantics ---

func TestConfProjGetSingle(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "top", nil)
		wantAttrs(t, item, "top") // keys are NOT auto-returned (spec §2.1.1)
	})
}

func TestConfProjGetMultiple(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "top, num", nil)
		wantAttrs(t, item, "num", "top")
	})
}

func TestConfProjGetMissing(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "nonexistent, top", nil)
		wantAttrs(t, item, "top") // missing path omitted, no error
	})
}

func TestConfProjGetNestedSpine(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "obj.nested.x", nil)
		wantAttrs(t, item, "obj")
		obj := item["obj"].(*types.AttributeValueMemberM).Value
		wantAttrs(t, obj, "nested")
		nested := obj["nested"].(*types.AttributeValueMemberM).Value
		wantAttrs(t, nested, "x")
		if nested["x"].(*types.AttributeValueMemberS).Value != "xval" {
			t.Errorf("obj.nested.x = %v, want xval", nested["x"])
		}
	})
}

func TestConfProjGetSiblingNested(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "obj.a, obj.b", nil)
		wantAttrs(t, item, "obj")
		obj := item["obj"].(*types.AttributeValueMemberM).Value
		wantAttrs(t, obj, "a", "b")
	})
}

func TestConfProjGetListElement(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "arr[1]", nil)
		wantAttrs(t, item, "arr")
		arr := item["arr"].(*types.AttributeValueMemberL).Value
		if len(arr) != 1 || arr[0].(*types.AttributeValueMemberS).Value != "e1" {
			t.Errorf("arr = %v, want single-element [e1] (compacted)", arr)
		}
	})
}

func TestConfProjGetNameSubstitution(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "#t, #o.#a", map[string]string{"#t": "top", "#o": "obj", "#a": "a"})
		wantAttrs(t, item, "obj", "top")
		obj := item["obj"].(*types.AttributeValueMemberM).Value
		wantAttrs(t, obj, "a")
	})
}

func TestConfProjGetReservedWord(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		putConf(t, c, ctx, "ProjT", map[string]types.AttributeValue{"pk": strVal("P1"), "name": strVal("reserved")})

		// Bare reserved word -> ValidationException.
		_, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:            aws.String("ProjT"),
			Key:                  map[string]types.AttributeValue{"pk": strVal("P1")},
			ProjectionExpression: aws.String("name"),
		})
		asValidation(t, err, "bare reserved word in projection")

		// Via #name alias -> accepted.
		item := projGetConf(t, c, ctx, "ProjT", "#n", map[string]string{"#n": "name"})
		wantAttrs(t, item, "name")
	})
}

func TestConfProjGetEmptyExpression(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		_, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:            aws.String("ProjT"),
			Key:                  map[string]types.AttributeValue{"pk": strVal("P1")},
			ProjectionExpression: aws.String(""),
		})
		asValidation(t, err, "empty ProjectionExpression")
	})
}

func TestConfProjGetPlusAttributesToGet(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		_, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:            aws.String("ProjT"),
			Key:                  map[string]types.AttributeValue{"pk": strVal("P1")},
			ProjectionExpression: aws.String("top"),
			AttributesToGet:      []string{"num"},
		})
		asValidation(t, err, "ProjectionExpression + AttributesToGet")
	})
}

// --- projection overlap rejection ---

func TestConfProjOverlapDuplicate(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		_, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:            aws.String("ProjT"),
			Key:                  map[string]types.AttributeValue{"pk": strVal("P1")},
			ProjectionExpression: aws.String("top, top"),
		})
		asValidation(t, err, "duplicate projection path")
	})
}

func TestConfProjOverlapParentChild(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		_, err := c.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:            aws.String("ProjT"),
			Key:                  map[string]types.AttributeValue{"pk": strVal("P1")},
			ProjectionExpression: aws.String("obj, obj.a"),
		})
		asValidation(t, err, "parent+child projection paths")
	})
}

// --- Query/Scan projection ---

func TestConfProjQuery(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 5) // sk 0..4, flag yes/no

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ProjectionExpression:      aws.String("flag"),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.Count != 5 || out.ScannedCount != 5 {
			t.Errorf("Count=%d ScannedCount=%d, want 5/5 (projection does not change counts)", out.Count, out.ScannedCount)
		}
		for i, item := range out.Items {
			wantAttrs(t, item, "flag")
			want := "no"
			if i%2 == 0 {
				want = "yes"
			}
			if v := item["flag"].(*types.AttributeValueMemberS).Value; v != want {
				t.Errorf("item[%d].flag = %q, want %q", i, v, want)
			}
		}
	})
}

func TestConfProjQueryAfterFilter(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 2) // sk 0 (flag yes), sk 1 (flag no)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		filterExpr := expression.Name("flag").Equal(expression.Value("yes"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr).WithFilter(filterExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			FilterExpression:          expr.Filter(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ProjectionExpression:      aws.String("flag"),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.Count != 1 || out.ScannedCount != 2 {
			t.Errorf("Count=%d ScannedCount=%d, want 1/2 (projection after filter)", out.Count, out.ScannedCount)
		}
		if len(out.Items) != 1 {
			t.Fatalf("Items = %d, want 1", len(out.Items))
		}
		wantAttrs(t, out.Items[0], "flag")
	})
}

func TestConfProjScan(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		out, err := c.Scan(ctx, &dynamodb.ScanInput{
			TableName:            aws.String("ConfT"),
			ProjectionExpression: aws.String("flag"),
		})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if out.Count != 3 {
			t.Fatalf("Count = %d, want 3", out.Count)
		}
		for _, item := range out.Items {
			wantAttrs(t, item, "flag")
		}
	})
}

func TestConfProjQuerySelectCountRejected(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Select:                    types.SelectCount,
			ProjectionExpression:      aws.String("flag"),
		})
		asValidation(t, err, "Select=COUNT + ProjectionExpression")
	})
}

func TestConfProjQuerySelectSpecificAttributes(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateComposite(t, c, ctx, "ConfT")
		seedComposite(t, c, ctx, "ConfT", "p1", 3)

		keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfT"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Select:                    types.SelectSpecificAttributes,
			ProjectionExpression:      aws.String("flag"),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if out.Count != 3 {
			t.Fatalf("Count = %d, want 3", out.Count)
		}
		for _, item := range out.Items {
			wantAttrs(t, item, "flag")
		}
	})
}

// --- GSI projection restriction ---

func TestConfProjGsiKeysOnlyRestricted(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfGsiT")
		seedGsiConformance(t, c, ctx, "ConfGsiT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		// Projecting a non-projected attr -> ValidationException.
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfGsiT"),
			IndexName:                 aws.String("gsi-keys"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ProjectionExpression:      aws.String("extra"),
		})
		asValidation(t, err, "KEYS_ONLY GSI project non-projected attr")

		// Projecting table key attrs -> accepted.
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfGsiT"),
			IndexName:                 aws.String("gsi-keys"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ProjectionExpression:      aws.String("pk, sk"),
		})
		if err != nil {
			t.Fatalf("KEYS_ONLY GSI project keys: %v", err)
		}
		if len(out.Items) != 3 {
			t.Fatalf("Items = %d, want 3 (G1 partition)", len(out.Items))
		}
		for _, item := range out.Items {
			wantAttrs(t, item, "pk", "sk")
		}
	})
}

func TestConfProjGsiInclude(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfGsiT")
		seedGsiConformance(t, c, ctx, "ConfGsiT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))

		// Included attr -> accepted. G1 partition has three items (A, B, C);
		// item C has NO proj1, so projecting proj1 on it yields an EMPTY item {}.
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfGsiT"),
			IndexName:                 aws.String("gsi-incl"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ProjectionExpression:      aws.String("proj1"),
		})
		if err != nil {
			t.Fatalf("INCLUDE GSI project proj1: %v", err)
		}
		if len(out.Items) != 3 {
			t.Fatalf("Items = %d, want 3 (G1 partition, includes C with no proj1)", len(out.Items))
		}
		for _, item := range out.Items {
			if len(item) == 0 {
				continue // item C projects proj1 -> empty map
			}
			wantAttrs(t, item, "proj1")
		}

		// Non-included attr -> ValidationException.
		_, err = c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfGsiT"),
			IndexName:                 aws.String("gsi-incl"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ProjectionExpression:      aws.String("extra"),
		})
		asValidation(t, err, "INCLUDE GSI project non-included attr")
	})
}

func TestConfProjGsiSelectAllProjectedPlusProjection(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfGsiT")
		seedGsiConformance(t, c, ctx, "ConfGsiT")

		keyExpr := expression.Key("gsi_pk").Equal(expression.Value("G1"))
		expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String("ConfGsiT"),
			IndexName:                 aws.String("gsi-incl"),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			Select:                    types.SelectAllProjectedAttributes,
			ProjectionExpression:      aws.String("proj1"),
		})
		asValidation(t, err, "ALL_PROJECTED_ATTRIBUTES + ProjectionExpression on GSI")
	})
}

func TestConfProjGsiScanRestricted(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateGsiTable(t, c, ctx, "ConfGsiT")
		seedGsiConformance(t, c, ctx, "ConfGsiT")

		_, err := c.Scan(ctx, &dynamodb.ScanInput{
			TableName:            aws.String("ConfGsiT"),
			IndexName:            aws.String("gsi-keys"),
			ProjectionExpression: aws.String("extra"),
		})
		asValidation(t, err, "KEYS_ONLY GSI Scan project non-projected attr")
	})
}

// --- projection edge cases ---

func TestConfProjDescendIntoScalar(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")
		item := projGetConf(t, c, ctx, "ProjT", "top.nested", nil)
		if len(item) != 0 {
			t.Errorf("attrs = %v, want none (path into a String does not resolve)", itemAttrNamesConf(item))
		}
	})
}

func TestConfProjListMultiIndex(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		projConfSeed(t, c, ctx, "ProjT")

		get := func(expr string) []string {
			item := projGetConf(t, c, ctx, "ProjT", expr, nil)
			arr := item["arr"].(*types.AttributeValueMemberL).Value
			got := make([]string, 0, len(arr))
			for _, e := range arr {
				got = append(got, e.(*types.AttributeValueMemberS).Value)
			}
			return got
		}
		// Both path orders yield the two elements, compacted; order per the
		// source-index order: arr[2], arr[0] -> [e0, e2]).
		if got := get("arr[0], arr[2]"); len(got) != 2 || got[0] != "e0" || got[1] != "e2" {
			t.Errorf("arr[0], arr[2] = %v, want [e0 e2]", got)
		}
		if got := get("arr[2], arr[0]"); len(got) != 2 || got[0] != "e0" || got[1] != "e2" {
			t.Errorf("arr[2], arr[0] = %v, want [e0 e2] (source-index order)", got)
		}
	})
}

func TestConfProjListConvergent(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "ProjT")
		putConf(t, c, ctx, "ProjT", map[string]types.AttributeValue{
			"pk": strVal("P1"),
			"marr": &types.AttributeValueMemberL{Value: []types.AttributeValue{
				&types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"x": strVal("x0"), "y": strVal("y0")}},
				&types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"x": strVal("x1"), "y": strVal("y1")}},
			}},
		})

		item := projGetConf(t, c, ctx, "ProjT", "marr[1].x, marr[1].y", nil)
		arr := item["marr"].(*types.AttributeValueMemberL).Value
		if len(arr) != 1 {
			t.Fatalf("marr = %v, want one merged element", arr)
		}
		elem := arr[0].(*types.AttributeValueMemberM).Value
		wantAttrs(t, elem, "x", "y")
		if elem["x"].(*types.AttributeValueMemberS).Value != "x1" || elem["y"].(*types.AttributeValueMemberS).Value != "y1" {
			t.Errorf("elem = %v, want {x:x1, y:y1}", elem)
		}
	})
}

// =====================================================================
// Expression-limit conformance (dual-target)
// =====================================================================

// TestConfExprStringLimit pins the 4KB expression-string byte-length limit.
// The ConditionExpression is 4097 bytes (4092 'a's plus " = :v"), which
// dynamodb-local 3.3.1 rejects with ValidationException "Expression size has
// exceeded the maximum allowed size"; the engine rejects the same input via
// its maxExprString check.
func TestConfExprStringLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Expr")

		longAttr := strings.Repeat("a", 4092)
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Expr"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String(longAttr + " = :v"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
		})
		asValidation(t, err, "Expression size")
	})
}

// TestConfOperatorCountLimit pins the 300-operator cap. 151 "a=a" comparisons
// joined by 150 " OR " yield 301 operators total, rejected.
func TestConfOperatorCountLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Expr")

		ops := make([]string, 151)
		for i := range ops {
			ops[i] = "a=a"
		}
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String("Expr"),
			Item:                map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression: aws.String(strings.Join(ops, " OR ")),
		})
		asValidation(t, err, "operator count")
	})
}

// TestConfInOperandLimit pins the 100-operand IN cap: 101 operands rejected.
func TestConfInOperandLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Expr")

		ops := make([]string, 101)
		for i := range ops {
			ops[i] = ":v"
		}
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Expr"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String("a IN (" + strings.Join(ops, ",") + ")"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
		})
		asValidation(t, err, "number of operands")
	})
}

// TestConfPathDepthLimit pins the 32-segment path cap: a 33-segment path in
// attribute_exists is rejected during binder.path resolution.
func TestConfPathDepthLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Expr")

		segs := make([]string, 33)
		for i := range segs {
			segs[i] = "a"
		}
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String("Expr"),
			Item:                map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression: aws.String("attribute_exists(" + strings.Join(segs, ".") + ")"),
		})
		asValidation(t, err, "nesting levels")
	})
}

// TestConfNameTokenLimit pins the 255-byte ExpressionAttributeNames KEY limit.
// The #name token ("#" + 255 'a's = 256 bytes) is rejected.
func TestConfNameTokenLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Expr")

		longName := "#" + strings.Repeat("a", 255)
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Expr"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String(longName + " = :v"),
			ExpressionAttributeNames:  map[string]string{longName: "attr"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
		})
		asValidation(t, err, "key too long")
	})
}

// TestConfSubstitutionValueLimit pins the ~1MB serialized
// ExpressionAttributeValues cap: a 2MB string value is rejected.
func TestConfSubstitutionValueLimit(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "Expr")

		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String("Expr"),
			Item:                      map[string]types.AttributeValue{"pk": strVal("k")},
			ConditionExpression:       aws.String("a = :v"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal(strings.Repeat("x", 2<<20))},
		})
		asValidation(t, err, "Expression size")
	})
}

// Every table-taking operation rejects an empty TableName with
// ValidationException.
func TestConfEmptyTableNameRejected(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		empty := aws.String("")
		ks := []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}}
		ads := []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}}
		key := map[string]types.AttributeValue{"pk": strVal("k")}

		check := func(op string, err error) {
			t.Helper()
			asValidation(t, err, op)
		}

		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{TableName: empty, KeySchema: ks, AttributeDefinitions: ads, BillingMode: types.BillingModePayPerRequest})
		check("CreateTable", err)
		_, err = c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: empty})
		check("DescribeTable", err)
		_, err = c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: empty})
		check("DeleteTable", err)
		_, err = c.PutItem(ctx, &dynamodb.PutItemInput{TableName: empty, Item: key})
		check("PutItem", err)
		_, err = c.GetItem(ctx, &dynamodb.GetItemInput{TableName: empty, Key: key})
		check("GetItem", err)
		_, err = c.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: empty, Key: key})
		check("DeleteItem", err)
		_, err = c.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: empty, Key: key, UpdateExpression: aws.String("SET #v = :v"), ExpressionAttributeNames: map[string]string{"#v": "v"}, ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")}})
		check("UpdateItem", err)
		_, err = c.Query(ctx, &dynamodb.QueryInput{TableName: empty, KeyConditionExpression: aws.String("pk = :pk"), ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strVal("k")}})
		check("Query", err)
		_, err = c.Scan(ctx, &dynamodb.ScanInput{TableName: empty})
		check("Scan", err)
		_, err = c.UpdateTable(ctx, &dynamodb.UpdateTableInput{TableName: empty, BillingMode: types.BillingModeProvisioned, ProvisionedThroughput: &types.ProvisionedThroughput{ReadCapacityUnits: aws.Int64(1), WriteCapacityUnits: aws.Int64(1)}})
		check("UpdateTable", err)
		_, err = c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{TableName: empty, TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("ttl")}})
		check("UpdateTimeToLive", err)
		_, err = c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: empty})
		check("DescribeTimeToLive", err)
		_, err = c.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{"": {batchPut(key)}}})
		check("BatchWriteItem", err)
		_, err = c.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{"": {Keys: []map[string]types.AttributeValue{key}}}})
		check("BatchGetItem", err)
	})
}

// mustCreateCompositeS creates a table with pk HASH S, sk RANGE S.
func mustCreateCompositeS(t *testing.T, c api, ctx context.Context, name string) {
	t.Helper()
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(name),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable %q: %v", name, err)
	}
}

// pk <= 2048 / sk <= 1024 bytes, inclusive; empty primary-key values
// rejected.
func TestConfKeyValueLengthLimits(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreateCompositeS(t, c, ctx, "KvLen")

		put := func(pk, sk string) error {
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName: aws.String("KvLen"),
				Item:      map[string]types.AttributeValue{"pk": strVal(pk), "sk": strVal(sk)},
			})
			return err
		}

		if err := put(strings.Repeat("k", 2048), "s"); err != nil {
			t.Errorf("pk 2048: %v", err)
		}
		asValidation(t, put(strings.Repeat("k", 2049), "s"), "pk 2049")
		if err := put("k2", strings.Repeat("s", 1024)); err != nil {
			t.Errorf("sk 1024: %v", err)
		}
		asValidation(t, put("k2", strings.Repeat("s", 1025)), "sk 1025")
		asValidation(t, put("", "s"), "empty pk")
		asValidation(t, put("k3", ""), "empty sk")
	})
}

// GSI key attribute names and INCLUDE NonKeyAttributes names are capped at
// 255 bytes.
func TestConfIndexAttrNameLengths(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		name256 := strings.Repeat("g", 256)

		_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("IdxLen"),
			KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String(name256), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("gsi1"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String(name256), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			}},
			BillingMode: types.BillingModePayPerRequest,
		})
		asValidation(t, err, "gsi key attr 256")

		_, err = c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName:            aws.String("InclLen"),
			KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("gsi1"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeInclude, NonKeyAttributes: []string{strings.Repeat("a", 256)}},
			}},
			BillingMode: types.BillingModePayPerRequest,
		})
		asValidation(t, err, "include attr 256")
	})
}

// User-specified projected attributes across all GSIs sum to <= 100; key
// attributes do not count.
func TestConfCrossIndexProjectionSum(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		attrs := func(prefix string, n int) []string {
			out := make([]string, n)
			for i := range out {
				out[i] = fmt.Sprintf("%s%03d", prefix, i)
			}
			return out
		}
		inclGsi := func(name string, projected []string) types.GlobalSecondaryIndex {
			return types.GlobalSecondaryIndex{
				IndexName:  aws.String(name),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeInclude, NonKeyAttributes: projected},
			}
		}
		base := func(table string, gsis ...types.GlobalSecondaryIndex) *dynamodb.CreateTableInput {
			return &dynamodb.CreateTableInput{
				TableName:              aws.String(table),
				KeySchema:              []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
				AttributeDefinitions:   []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
				GlobalSecondaryIndexes: gsis,
				BillingMode:            types.BillingModePayPerRequest,
			}
		}

		// 50 + 50 = 100: accepted.
		if _, err := c.CreateTable(ctx, base("SumOk", inclGsi("sumg1", attrs("a", 50)), inclGsi("sumg2", attrs("b", 50)))); err != nil {
			t.Errorf("sum 100: %v", err)
		}
		// 51 + 50 = 101: rejected.
		_, err := c.CreateTable(ctx, base("SumOver", inclGsi("sumg1", attrs("a", 51)), inclGsi("sumg2", attrs("b", 50))))
		asValidation(t, err, "sum 101")
		// 99 INCLUDE attrs + key attrs: accepted (key attrs do not count).
		if _, err := c.CreateTable(ctx, base("SumKeys", inclGsi("sumg1", attrs("a", 99)))); err != nil {
			t.Errorf("sum 99 + keys: %v", err)
		}
	})
}

// =====================================================================
// DescribeTable ItemCount/TableSizeBytes reporting (dual-target)
// =====================================================================

// DescribeTable reports real, immediate ItemCount/TableSizeBytes after puts,
// overwrites, and deletes (exact accounting sums; immediate — no eventual
// consistency in dynamodb-local).
func TestConfDescribeTableStats(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		mustCreate(t, c, ctx, "DescT")
		// Empty table: 0/0.
		desc, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("DescT")})
		if err != nil {
			t.Fatalf("DescribeTable empty: %v", err)
		}
		if aws.ToInt64(desc.Table.ItemCount) != 0 || aws.ToInt64(desc.Table.TableSizeBytes) != 0 {
			t.Errorf("empty = (count %d, size %d), want (0, 0)", aws.ToInt64(desc.Table.ItemCount), aws.ToInt64(desc.Table.TableSizeBytes))
		}
		// {pk:k1,gp:G1}=8, {pk:k2,gp:G1}=8.
		putConf(t, c, ctx, "DescT", map[string]types.AttributeValue{"pk": sv("k1"), "gp": sv("G1")})
		putConf(t, c, ctx, "DescT", map[string]types.AttributeValue{"pk": sv("k2"), "gp": sv("G1")})
		desc, _ = c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("DescT")})
		if aws.ToInt64(desc.Table.ItemCount) != 2 || aws.ToInt64(desc.Table.TableSizeBytes) != 16 {
			t.Errorf("after puts = (count %d, size %d), want (2, 16)", aws.ToInt64(desc.Table.ItemCount), aws.ToInt64(desc.Table.TableSizeBytes))
		}
		// Overwrite k1 larger: {pk:k1,gp:G1,extra:hello}=18; total 18+8=26.
		putConf(t, c, ctx, "DescT", map[string]types.AttributeValue{"pk": sv("k1"), "gp": sv("G1"), "extra": sv("hello")})
		desc, _ = c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("DescT")})
		if aws.ToInt64(desc.Table.ItemCount) != 2 || aws.ToInt64(desc.Table.TableSizeBytes) != 26 {
			t.Errorf("after overwrite = (count %d, size %d), want (2, 26)", aws.ToInt64(desc.Table.ItemCount), aws.ToInt64(desc.Table.TableSizeBytes))
		}
		// Delete k2: remaining 18.
		if _, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String("DescT"),
			Key:       map[string]types.AttributeValue{"pk": sv("k2")},
		}); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		desc, _ = c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("DescT")})
		if aws.ToInt64(desc.Table.ItemCount) != 1 || aws.ToInt64(desc.Table.TableSizeBytes) != 18 {
			t.Errorf("after delete = (count %d, size %d), want (1, 18)", aws.ToInt64(desc.Table.ItemCount), aws.ToInt64(desc.Table.TableSizeBytes))
		}
	})
}

// TestConfDescribeTableGsiStats: per-GSI ItemCount/IndexSizeBytes are
// projection-independent (IndexSizeBytes = full-item sum over indexed items
// regardless of projection), sparse items excluded, and values immediate.
func TestConfDescribeTableGsiStats(t *testing.T) {
	runConformance(t, func(t *testing.T, c api) {
		ctx := context.Background()
		// Table with three GSIs on gp: ALL, KEYS_ONLY, INCLUDE.
		if _, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("GsiT"),
			KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
				{IndexName: aws.String("g-all"), KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}}, Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll}},
				{IndexName: aws.String("g-keys"), KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}}, Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly}},
				{IndexName: aws.String("g-incl"), KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}}, Projection: &types.Projection{ProjectionType: types.ProjectionTypeInclude, NonKeyAttributes: []string{"proj1"}}},
			},
			BillingMode: types.BillingModePayPerRequest,
		}); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		// Wait for all GSIs to be ACTIVE before writing (no-op on the adapter).
		for _, g := range []string{"g-all", "g-keys", "g-incl"} {
			waitForGsiActive(t, c, ctx, "GsiT", g)
		}
		// {pk:a,gp:G1,proj1:x}=13, {pk:bb,gp:G1,proj1:yy}=15, {pk:ccc}=5 (sparse).
		putConf(t, c, ctx, "GsiT", map[string]types.AttributeValue{"pk": sv("a"), "gp": sv("G1"), "proj1": sv("x")})
		putConf(t, c, ctx, "GsiT", map[string]types.AttributeValue{"pk": sv("bb"), "gp": sv("G1"), "proj1": sv("yy")})
		putConf(t, c, ctx, "GsiT", map[string]types.AttributeValue{"pk": sv("ccc")})
		desc, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("GsiT")})
		if err != nil {
			t.Fatalf("DescribeTable: %v", err)
		}
		// Table: 3 items, 13+15+5=33 bytes.
		if aws.ToInt64(desc.Table.ItemCount) != 3 || aws.ToInt64(desc.Table.TableSizeBytes) != 33 {
			t.Errorf("table = (count %d, size %d), want (3, 33)", aws.ToInt64(desc.Table.ItemCount), aws.ToInt64(desc.Table.TableSizeBytes))
		}
		// Each GSI: 2 indexed (ccc sparse), 13+15=28 bytes (projection-independent).
		for _, g := range desc.Table.GlobalSecondaryIndexes {
			if aws.ToInt64(g.ItemCount) != 2 {
				t.Errorf("GSI %q count = %d, want 2", aws.ToString(g.IndexName), aws.ToInt64(g.ItemCount))
			}
			if aws.ToInt64(g.IndexSizeBytes) != 28 {
				t.Errorf("GSI %q size = %d, want 28", aws.ToString(g.IndexName), aws.ToInt64(g.IndexSizeBytes))
			}
		}
	})
}
