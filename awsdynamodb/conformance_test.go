package awsdynamodb_test

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

// mustCreateGsiTable builds the M4 GSI conformance table: pk HASH S, sk RANGE S,
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

// seedGsiConformance puts the five M4 seed items into the table.
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

// --- M3 conformance cases (Query/Scan) ---

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

// TestConfParallelScan (case 27) verifies that a scan split into TotalSegments
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

// TestConfQueryBeginsWithOnStringSortKey covers the begins_with-on-S gap in
// case 17 (TestConfQuerySortKeyConditions only exercised =,<,<=,>,>=,BETWEEN
// on an N sort key). It creates a composite table with an S sort key and
// asserts begins_with(sk, :prefix) returns exactly the prefix-matching items.
// This is the case that would have caught the P0 bug (the S-key upper-bound
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

		// Key carrying extra attributes (spec case 30).
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

		_, isAdapter := c.(*awsdynamodb.Adapter)
		_, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName: aws.String("ConfT"),
			KeyConditions: map[string]types.Condition{
				"pk": {ComparisonOperator: types.ComparisonOperatorEq, AttributeValueList: []types.AttributeValue{strVal("p1")}},
			},
		})
		if isAdapter {
			// M3 deliberately does not implement the deprecated pre-expression
			// parameters; the adapter rejects a non-empty KeyConditions with a
			// ValidationException so a caller never believes the constraint was
			// applied. This is an adapter scope decision (see design spec §7.5).
			asValidation(t, err, "legacy KeyConditions should be rejected on the adapter")
			return
		}
		// The reference (dynamodb-local 3.3.1) still accepts the deprecated
		// KeyConditions parameter (returns the items). The reference wins over
		// the spec's §7.5 "reject legacy params" claim: this divergence is
		// documented in the design spec §7.5, not fixed in the engine.
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

// --- M4 GSI conformance cases (cases 38-52, spec §10.3) ---

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

// Case 38: Basic GSI Query — IndexName + gsi_pk = :v returns the right items in
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

// Case 39: Sparse GSI — D has no GSI attributes, so it is absent from both a
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

// Case 40: Non-unique GSI key — A and C share gsi_sk=s1 under gsi_pk=G1 and
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

// Case 41: GSI sort-key conditions — =, <, <=, >, >=, BETWEEN and begins_with
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

// Case 42: ScanIndexForward=false on a GSI returns items in descending GSI sort
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

// Case 43: GSI pagination — Limit=2 with resume to exhaustion.
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

// Case 44: ConsistentRead=true is rejected on a GSI Query and Scan.
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

// Case 45: KEYS_ONLY projection — a query on gsi-keys returns only the table
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

// Case 46: INCLUDE projection — gsi-incl returns table keys + GSI keys + the
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

// Case 47: ALL projection — a query on gsi-all returns every attribute.
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

// Case 48: Select=ALL_PROJECTED_ATTRIBUTES on a GSI returns the projected attrs.
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

// Case 49: Select=ALL_ATTRIBUTES on a non-ALL GSI is a ValidationException.
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

// Case 50: a non-GSI key attribute in KeyConditionExpression is a
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

// Case 51: GSI Scan — gsi-all returns every indexed item (D excluded) with a
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

// Case 52: GSI Scan pagination — Limit=2 with resume to exhaustion.
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

// Case 53: begins_with on the GSI sort key performs a prefix match.
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

// Case 54: UpdateItem changes a GSI key — the item moves to the new GSI
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

// Case 55: UpdateItem removes a GSI key — the item becomes sparse (absent from
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

// Case 56: DeleteItem removes the item from the GSI.
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

// Case 57: a partition-only GSI — gsi_pk = :v returns every item in that
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

// Case 58: GSI partition key equal to the table partition key (overlapping key)
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

// Case 59: an ExclusiveStartKey whose GSI partition does not match the
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

// Case 60: an item with gsi_pk but no gsi_sk (composite GSI) is accepted but
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

// Case 61: GSI key type mismatch on PutItem — gsi_pk as Number — is a
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

// Case 62: a non-scalar GSI key attribute (L/BOOL/SS/NULL) on PutItem is a
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

// Case 63: GSI key type mismatch on UpdateItem (SET gsi_pk = :n) is a
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

// Case 64: an empty string as a GSI partition key value is a
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

// Case 65: GSI ExclusiveStartKey shape validation — table-only, GSI-only, and
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

// Case 66: a duplicate AttributeDefinition is a ValidationException, with and
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

// Case 67: GSI IndexName validation — illegal characters and names too short
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

// Case 68: DescribeTable returns the GSI defs — key schemas round-trip.
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

// --- M5a TTL conformance cases ---

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

		// Nonexistent table -> ResourceNotFoundException (precedence over spec validation).
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
// M5b — BatchWriteItem conformance (dual-target)
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

// Confirmed permanent dual-target cases (spec §6.1; Task 1 probes returned
// ValidationException for all three against dynamodb-local 3.3.1).

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
// M5b — BatchGetItem conformance (dual-target)
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

// Confirmed permanent dual-target (spec §2.4; Task 1 probe returned
// ValidationException for empty Keys against dynamodb-local 3.3.1).
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

// §6.4: TTL Faithful read model — expired items are visible to BatchGetItem.
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
			t.Errorf("len(Responses[T]) = %d, want 1 (expired item visible, M5a Faithful model)", len(out.Responses["BatchT"]))
		}
	})
}
