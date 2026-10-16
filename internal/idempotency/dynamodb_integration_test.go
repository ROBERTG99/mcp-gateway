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

func TestDynamoDBStore(t *testing.T) {
	endpoint := dynamoEndpoint(t)
	client := dynamodb.New(dynamodb.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	var n atomic.Int32
	storetest.Run(t, func(t *testing.T) storetest.Harness {
		c := clock.NewFake(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))
		s := idempotency.NewDynamoDB(client, fmt.Sprintf("mcpgw_idempotency_%d", n.Add(1)), c)
		if err := s.EnsureTable(t.Context()); err != nil {
			t.Fatal(err)
		}
		return storetest.Harness{Store: s, Advance: c.Advance}
	})
}
