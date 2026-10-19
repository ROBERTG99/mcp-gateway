package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// DynamoDB is the optional Store backend: table mcpgw_idempotency, partition key pk =
// "{tenant}#{key}", TTL attribute expires_at (epoch seconds). Every transition is a conditional
// write; expiry is enforced in the conditions (expires_at_ms) because DynamoDB deletes expired
// items lazily.
type DynamoDB struct {
	client *dynamodb.Client
	table  string
	clock  clock.Clock
}

// NewDynamoDB returns the store for table.
func NewDynamoDB(client *dynamodb.Client, table string, c clock.Clock) *DynamoDB {
	if c == nil {
		c = clock.Real{}
	}
	return &DynamoDB{client: client, table: table, clock: c}
}

func dynamoKey(tenant, key string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: tenant + "#" + key}}
}

func numMs(t time.Time) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(t.UnixMilli(), 10)}
}

// EnsureTable creates the table (on-demand billing) and enables TTL when it does not exist.
func (d *DynamoDB) EnsureTable(ctx context.Context) error {
	_, err := d.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(d.table)})
	if err == nil {
		return nil
	}
	var nf *types.ResourceNotFoundException
	if !errors.As(err, &nf) {
		return fmt.Errorf("describe table: %w", err)
	}
	_, err = d.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(d.table),
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		BillingMode:          types.BillingModePayPerRequest,
	})
	var inUse *types.ResourceInUseException
	if err != nil && !errors.As(err, &inUse) {
		return fmt.Errorf("create table: %w", err)
	}
	_, err = d.client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{TableName: aws.String(d.table),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{AttributeName: aws.String("expires_at"), Enabled: aws.Bool(true)}})
	if err != nil {
		return fmt.Errorf("enable ttl: %w", err)
	}
	return nil
}

func isConditionFailed(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func (d *DynamoDB) item(tenant, key string, rec record, expires time.Time) map[string]types.AttributeValue {
	it := dynamoKey(tenant, key)
	it["state"] = &types.AttributeValueMemberS{Value: rec.State}
	it["args_sha256"] = &types.AttributeValueMemberS{Value: rec.ArgsHash}
	it["expires_at"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(expires.Unix()+1, 10)}
	it["expires_at_ms"] = numMs(expires)
	if rec.Result != nil {
		it["result"] = &types.AttributeValueMemberS{Value: string(rec.Result)}
	}
	return it
}

// Acquire implements Store.
func (d *DynamoDB) Acquire(ctx context.Context, tenant, key, argsHash string, lockTTL time.Duration) (Acquisition, error) {
	now := d.clock.Now()
	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 aws.String(d.table),
		Item:                      d.item(tenant, key, record{State: stateInProgress, ArgsHash: argsHash}, now.Add(lockTTL)),
		ConditionExpression:       aws.String("attribute_not_exists(pk) OR expires_at_ms <= :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":now": numMs(now)},
	})
	if err == nil {
		return Acquisition{Outcome: Acquired}, nil
	}
	if !isConditionFailed(err) {
		return Acquisition{}, fmt.Errorf("dynamodb acquire: %w", err)
	}
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.table), Key: dynamoKey(tenant, key), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return Acquisition{}, fmt.Errorf("dynamodb acquire: read: %w", err)
	}
	rec, ok := decodeItem(out.Item)
	if !ok {
		return Acquisition{Outcome: InProgress}, nil // vanished between the writes: let the caller retry
	}
	return decide(rec, argsHash), nil
}

func decodeItem(it map[string]types.AttributeValue) (record, bool) {
	if it == nil {
		return record{}, false
	}
	var r record
	if v, ok := it["state"].(*types.AttributeValueMemberS); ok {
		r.State = v.Value
	}
	if v, ok := it["args_sha256"].(*types.AttributeValueMemberS); ok {
		r.ArgsHash = v.Value
	}
	if v, ok := it["result"].(*types.AttributeValueMemberS); ok {
		r.Result = json.RawMessage(v.Value)
	}
	return r, true
}

// Complete implements Store.
func (d *DynamoDB) Complete(ctx context.Context, tenant, key, argsHash string, result json.RawMessage, ttl time.Duration) error {
	now := d.clock.Now()
	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                aws.String(d.table),
		Item:                     d.item(tenant, key, record{State: stateCompleted, ArgsHash: argsHash, Result: result}, now.Add(ttl)),
		ConditionExpression:      aws.String("#s = :inprog AND args_sha256 = :h AND expires_at_ms > :now"),
		ExpressionAttributeNames: map[string]string{"#s": "state"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":inprog": &types.AttributeValueMemberS{Value: stateInProgress},
			":h":      &types.AttributeValueMemberS{Value: argsHash},
			":now":    numMs(now),
		},
	})
	if isConditionFailed(err) {
		return ErrNotOwner
	}
	if err != nil {
		return fmt.Errorf("dynamodb complete: %w", err)
	}
	return nil
}

// Release implements Store.
func (d *DynamoDB) Release(ctx context.Context, tenant, key, argsHash string) error {
	_, err := d.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                aws.String(d.table),
		Key:                      dynamoKey(tenant, key),
		ConditionExpression:      aws.String("#s = :inprog AND args_sha256 = :h"),
		ExpressionAttributeNames: map[string]string{"#s": "state"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":inprog": &types.AttributeValueMemberS{Value: stateInProgress},
			":h":      &types.AttributeValueMemberS{Value: argsHash},
		},
	})
	if err != nil && !isConditionFailed(err) {
		return fmt.Errorf("dynamodb release: %w", err)
	}
	return nil
}

// Ping implements Store.
func (d *DynamoDB) Ping(ctx context.Context) error {
	if _, err := d.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(d.table)}); err != nil {
		return fmt.Errorf("dynamodb ping: %w", err)
	}
	return nil
}
