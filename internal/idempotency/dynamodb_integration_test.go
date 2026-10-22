//go:build integration

package idempotency_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency/storetest"
)

func dynamoEndpoint(t *testing.T) string {
	t.Helper()
	if os.Getenv("MCPGW_TEST_USE_COMPOSE") == "1" {
		if ep := os.Getenv("MCPGW_TEST_DYNAMODB_ENDPOINT"); ep != "" {
			return ep
		}
		return "http://localhost:8240"
	}
	ctx := context.Background()
	c, err := testcontainers.Run(ctx, "amazon/dynamodb-local:3.3.2",
		testcontainers.WithExposedPorts("8000/tcp"),
		testcontainers.WithCmd("-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("8000/tcp").WithStartupTimeout(2*time.Minute)))
	if err != nil {
		t.Fatalf("start dynamodb-local: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	ep, err := c.PortEndpoint(ctx, "8000/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

// liveStart is the fake clock's start for DynamoDB tests: the current time, not a fixed date. The
// table enables DynamoDB TTL on expires_at, and dynamodb-local's sweeper deletes items whose
// expires_at is in the real past; with a fixed past date it deleted live locks mid-test, letting a
// second caller acquire the same key (the "concurrent acquire" case failed with 2 winners of 24).
func liveStart() time.Time { return time.Now().UTC().Truncate(time.Second) }

func dynamoClient(endpoint string) *dynamodb.Client {
	return dynamodb.New(dynamodb.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
}

// runID tells this test binary's tables apart from those of earlier runs: with
// MCPGW_TEST_USE_COMPOSE the dynamodb-local service outlives the binary (CI runs make integration,
// then make coverage, against the same service), and a reused table still holds the last run's
// records and locks.
var runID = time.Now().UnixNano()

// freshTable returns a table name unique to this run and deletes the table when t ends.
func freshTable(t *testing.T, client *dynamodb.Client, name string) string {
	t.Helper()
	table := fmt.Sprintf("%s_%d", name, runID)
	t.Cleanup(func() {
		_, _ = client.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	})
	return table
}

func TestDynamoDBStore(t *testing.T) {
	client := dynamoClient(dynamoEndpoint(t))
	var n atomic.Int32
	storetest.Run(t, func(t *testing.T) storetest.Harness {
		c := clock.NewFake(liveStart())
		s := idempotency.NewDynamoDB(client, freshTable(t, client, fmt.Sprintf("mcpgw_idempotency_%d", n.Add(1))), c)
		if err := s.EnsureTable(t.Context()); err != nil {
			t.Fatal(err)
		}
		return storetest.Harness{Store: s, Advance: c.Advance}
	})
}

// TestOpenDynamoDBLocalCreatesTable: with an endpoint override (dynamodb-local), Open uses static
// local credentials and creates the table, and the store then works.
func TestOpenDynamoDBLocalCreatesTable(t *testing.T) {
	c := clock.NewFake(liveStart())
	endpoint := dynamoEndpoint(t)
	table := freshTable(t, dynamoClient(endpoint), "mcpgw_idempotency_open")
	s, err := idempotency.Open(t.Context(), idempotency.OpenOptions{Backend: "dynamodb", DynamoDBEndpoint: endpoint,
		DynamoDBTable: table, DynamoDBRegion: "us-east-1", Clock: c})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	a, err := s.Acquire(t.Context(), "t_alpha", "key-00000001", "h", time.Minute)
	if err != nil || a.Outcome != idempotency.Acquired {
		t.Fatalf("acquire = %+v, %v", a, err)
	}
}
