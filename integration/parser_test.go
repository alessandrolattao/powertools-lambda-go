package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
	"github.com/alessandrolattao/powertools-lambda-go/parser"
)

type order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

// TestParserSNSInSQS publishes to an SNS topic with an SQS subscription and
// parses what the queue receives: the SNS notification as SNS really writes
// it into SQS, with its SigningCertURL and UnsubscribeURL spelling.
func TestParserSNSInSQS(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	name := fmt.Sprintf("parser-sns-sqs-%d", time.Now().UnixNano())
	snsClient := sns.NewFromConfig(suite.aws)
	sqsClient := sqs.NewFromConfig(suite.aws)

	topic, err := snsClient.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(name)})
	require.NoError(t, err)
	queue, err := sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name)})
	require.NoError(t, err)
	attrs, err := sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: queue.QueueUrl, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	require.NoError(t, err)
	queueARN := attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	_, err = snsClient.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(queueARN),
	})
	require.NoError(t, err)

	_, err = snsClient.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: aws.String(`{"id":"o-1","total":12.5}`)})
	require.NoError(t, err)

	var received []sqstypes.Message
	require.Eventually(t, func() bool {
		out, receiveErr := sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: queue.QueueUrl, MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		})
		if receiveErr != nil {
			return false
		}
		received = append(received, out.Messages...)
		return len(received) > 0
	}, 30*time.Second, 100*time.Millisecond)

	event := events.SQSEvent{}
	for _, m := range received {
		event.Records = append(event.Records, events.SQSMessage{
			MessageId: aws.ToString(m.MessageId), Body: aws.ToString(m.Body), EventSourceARN: queueARN,
		})
	}

	orders, err := parser.SNSSQS[order](event)
	require.NoError(t, err)
	assert.Equal(t, []order{{ID: "o-1", Total: 12.5}}, orders)

	notification, err := eventsource.NestedSNSMessage(event.Records[0])
	require.NoError(t, err)
	assert.Equal(t, aws.ToString(topic.TopicArn), notification.TopicArn)

	url, err := eventsource.SQSQueueURL(queueARN)
	require.NoError(t, err)
	assert.Contains(t, url, name)
}
