package idempotency

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/redis/go-redis/v9"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// OpenOptions select and configure a backend.
type OpenOptions struct {
	Backend          string // redis | dynamodb | memory
	Redis            redis.UniversalClient
	DynamoDBEndpoint string
	DynamoDBTable    string
	DynamoDBRegion   string
	Clock            clock.Clock
}

// Open builds the configured store. With a DynamoDB endpoint override (dynamodb-local) static
// local credentials are used and the table is created when missing; otherwise the default AWS
// credential chain applies and the table must exist.
func Open(ctx context.Context, o OpenOptions) (Store, error) {
	switch o.Backend {
	case "memory":
		return NewMemory(o.Clock), nil
	case "redis":
		if o.Redis == nil {
			return nil, fmt.Errorf("redis idempotency store needs a client")
		}
		return NewRedis(o.Redis), nil
	case "dynamodb":
		var client *dynamodb.Client
		if o.DynamoDBEndpoint != "" {
			client = dynamodb.New(dynamodb.Options{Region: o.DynamoDBRegion, BaseEndpoint: aws.String(o.DynamoDBEndpoint),
				Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")})
		} else {
			cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(o.DynamoDBRegion))
			if err != nil {
				return nil, fmt.Errorf("aws config: %w", err)
			}
			client = dynamodb.NewFromConfig(cfg)
		}
		s := NewDynamoDB(client, o.DynamoDBTable, o.Clock)
		if o.DynamoDBEndpoint != "" {
			if err := s.EnsureTable(ctx); err != nil {
				return nil, err
			}
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unknown idempotency store %q", o.Backend)
	}
}
